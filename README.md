# proxperfect

<img src="graphics/proxperfect-logo.svg" width="50%" height="50%" alt="proxperfect logo" align="center"/>

**A fan-out reverse proxy for HTTP (including S3)**

proxperfect proxides a single endpoint for HTTP clients to connect to and forwards incoming requests to a given set of servers in a round-robin fashion. This is a way to balance load across multiple servers for applications that have no native support for multiple endpoints and for cases in which DNS-based load balancing is not feasible.

## Usage

The built-in help (`proxperfect --help`) provides simple examples to get started.

You can get proxperfect pre-built for Linux from the [Releases section](https://github.com/breuner/proxperfect/releases) and from [Docker Hub](https://hub.docker.com/r/breuner/proxperfect). 

### Questions & Comments

In case of questions, comments, if something is missing to make proxperfect more useful or if you would just like to share your thoughts, feel free to contact me: sven.breuner[at]gmail.com

