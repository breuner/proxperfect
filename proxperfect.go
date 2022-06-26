// A reverse proxy that distributes load by forwarding different requests to different http servers

package main

import (
	"context"
	"flag"
	"fmt"
	"golang.org/x/sync/semaphore"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
)

const ProgName = "ProxPerfect"
const ProgVersion string = "1.0.1"

type Config struct {
	beVerbose         bool
	showVersion       bool
	listenPort        int
	proxyStrings      []string
	poolBufSize       int
	numConnsPerServer int    // 0 disables this limit
	redirectCode      int    // 0 disables redirect
	fdLimit           uint64 // 0 disables attempt to change
}

var config Config

type ProxyState struct {
	proxies      []*httputil.ReverseProxy
	connLimiters []*semaphore.Weighted // per-proxy limit
	requestNum   uint32
}

var proxyState ProxyState

// proxyBufferPool is a httputil.BufferPool backed by a thread-safe sync.Pool
// note: sync.Pool is garbage-collected on mem pressure, so doesn't need upper bound of elems
type proxyBufferPool struct {
	pool        *sync.Pool
	bufAllocNum uint32
}

func NewProxyBufferPool() httputil.BufferPool {
	return &proxyBufferPool{
		pool:        new(sync.Pool),
		bufAllocNum: 0,
	}
}

func (bufPool *proxyBufferPool) Get() []byte {
	buf := bufPool.pool.Get()
	if buf == nil {
		if config.beVerbose {
			var currentAllocNum = atomic.AddUint32(&bufPool.bufAllocNum, 1)

			fmt.Printf("Allocating proxy pool buf. Num: %d; Total alloc size: %d\n", currentAllocNum, uint32(config.poolBufSize)*currentAllocNum)
		}

		return make([]byte, config.poolBufSize)
	}

	return buf.([]byte)
}

func (bufPool *proxyBufferPool) Put(buf []byte) {
	bufPool.pool.Put(buf)
}

func Usage() {
	exename := filepath.Base(os.Args[0])

	fmt.Printf("proxperfect - A fan-out reverse proxy")

	fmt.Printf("Usage: ./%s [OPTIONS] HTTP_SERVERS...\n", exename)
	fmt.Println()

	fmt.Println("Options:")
	flag.PrintDefaults()
	fmt.Println()

	fmt.Printf("Example:\n")
	fmt.Printf("  Forward requests round-robin to servers 192.168.0.1 through 192.168.0.8:\n")
	fmt.Printf("    $ ./%s http://192.168.0.{1..8}\n", exename)
}

// parse command line args and init config struct
func ParseArguments() {
	showVersionConfigPtr := flag.Bool("version", false, "Print version and exit.")
	beVerboseConfigPtr := flag.Bool("verbose", false, "Print verbose output.")
	listenPortConfigPtr := flag.Int("port", 8080, "Port to listen on for incoming connections.")
	poolBufSizeConfigPtr := flag.Int("bufsize", 128*1024, "Size of each pooled buffer in bytes. [0 disables buffer pooling.]")
	numConnsPerServer := flag.Int("maxconns", 10, "Max number of connections per server. [0 disables limit.]")
	redirectCode := flag.Int("redirect", 0, "Redirect requests using given HTTP code instead of proxying. [0 disables redirect; 301 is temporary redirect.]")
	fdLimit := flag.Uint64("fdlimit", 0, "Increase open file descriptor limit of process (as in 'ulimit -n').")

	flag.Parse()

	config.beVerbose = *beVerboseConfigPtr
	config.showVersion = *showVersionConfigPtr
	config.listenPort = *listenPortConfigPtr
	config.poolBufSize = *poolBufSizeConfigPtr
	config.proxyStrings = flag.Args()
	config.numConnsPerServer = *numConnsPerServer
	config.redirectCode = *redirectCode
	config.fdLimit = *fdLimit

	if config.showVersion {
		fmt.Printf("%s v%s\n", ProgName, ProgVersion)
		os.Exit(0)
	}

	if config.beVerbose {
		fmt.Println("HTTP Servers:", flag.Args())
	}

	if len(flag.Args()) == 0 {
		fmt.Println("ERROR: HTTP servers missing. Specify one or more server arguments.")
		fmt.Println("       (Format: \"http://<host>:<port>\")")
		fmt.Println()
		Usage()

		os.Exit(1)
	}

	if config.fdLimit != 0 {
		SetOpenFilesLimit()
	}

	if config.beVerbose {
		rlimit := GetOpenFilesLimit()
		fmt.Printf("Current open files limit: %d (Max: %d)\n", rlimit.Cur, rlimit.Max)
	}

}

// get "ulimit -n"
func GetOpenFilesLimit() syscall.Rlimit {
	rlimit := syscall.Rlimit{Cur: 0, Max: 0}

	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit)

	if err != nil {
		fmt.Println("Unable to get limit for open file handles:", err)
	}

	return rlimit
}

// set "ulimit -n"
// this func will only increase the limit, not decrease
func SetOpenFilesLimit() {
	rlimit := GetOpenFilesLimit()

	if config.fdLimit >= rlimit.Max {
		fmt.Printf("Adjusting config for open files limit: Was exceeding max. (Current: %d; Max: %d; Config: %d)\n", rlimit.Cur, rlimit.Max, config.fdLimit)
		config.fdLimit = rlimit.Max
	}

	if rlimit.Cur >= config.fdLimit {
		fmt.Printf("Skipping increase of open files limit: Config value is not higher than current value (Current: %d; Config: %d)\n", rlimit.Cur, config.fdLimit)
		return
	}

	rlimit.Cur = config.fdLimit

	err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	if err != nil {
		fmt.Println("Setting open files limit failed:", err)
	}
}

// NewProxy takes target host and creates a reverse proxy
func NewProxy(targetHost string) (*httputil.ReverseProxy, error) {
	url, err := url.Parse(targetHost)
	if err != nil {
		return nil, err
	}

	return httputil.NewSingleHostReverseProxy(url), nil
}

func InitProxyState() {
	proxyState.requestNum = 0

	for i, proxyStr := range flag.Args() {
		if config.beVerbose {
			fmt.Printf("Adding proxy. Index: %d; Server: %s\n", i, proxyStr)
		}

		proxy, err := NewProxy(flag.Arg(i))
		if err != nil {
			panic(err)
		}

		proxy.FlushInterval = -1 // negative value means "flush immediately"

		if config.poolBufSize > 0 {
			proxy.BufferPool = NewProxyBufferPool()
		}

		proxyState.proxies = append(proxyState.proxies, proxy)

		if config.numConnsPerServer != 0 {
			var sem = semaphore.NewWeighted(int64(config.numConnsPerServer))
			proxyState.connLimiters = append(proxyState.connLimiters, sem)
		}
	}

}

// ProxyRequestHandler proxies the http request to server from given list
func ProxyRequestHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var currentRequestNum = atomic.AddUint32(&proxyState.requestNum, 1)
		var proxyIdx = currentRequestNum % uint32(len(proxyState.proxies))
		var proxy = proxyState.proxies[proxyIdx]
		var limiter *semaphore.Weighted

		// limit concurrent connections for this proxy
		if config.numConnsPerServer != 0 {
			limiter = proxyState.connLimiters[proxyIdx]
			ctx := context.Background()
			limiter.Acquire(ctx, 1)
		}

		if config.beVerbose {
			fmt.Printf("[%s START #%d]: %s %s\n", config.proxyStrings[proxyIdx], currentRequestNum, r.Method, r.URL.String())
		}

		proxy.ServeHTTP(w, r)

		if config.beVerbose {
			fmt.Printf("[%s END   #%d]: %s %s\n", config.proxyStrings[proxyIdx], currentRequestNum, r.Method, r.URL.String())
		}

		if config.numConnsPerServer != 0 {
			limiter.Release(1)
		}
	}
}

// RedirectHandler redirects incoming http request to server from given list
func RedirectHandler(w http.ResponseWriter, r *http.Request) {
	var currentRequestNum = atomic.AddUint32(&proxyState.requestNum, 1)
	var proxyIdx = currentRequestNum % uint32(len(proxyState.proxies))
	var serverStr = config.proxyStrings[proxyIdx] + r.URL.String()

	if config.beVerbose {
		fmt.Printf("[%s REDIRECT #%d]: %s %s\n", config.proxyStrings[proxyIdx], currentRequestNum, r.Method, r.URL.String())
	}

	http.Redirect(w, r, serverStr, config.redirectCode)
}

func main() {
	flag.Usage = Usage

	ParseArguments()

	InitProxyState()

	// register http request handler
	if config.redirectCode == 0 {
		// handle requests through proxy
		http.HandleFunc("/", ProxyRequestHandler())
	} else {
		// handle requests through redirector
		http.HandleFunc("/", RedirectHandler)
	}

	fmt.Printf("Listening on port %d...\n", config.listenPort)

	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(config.listenPort), nil))
}
