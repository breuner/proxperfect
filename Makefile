EXE ?= ./proxperfect

all: $(EXE)

$(EXE): $(EXE).go
	go build -o $(EXE) $(EXE).go

clean:
	rm -f $(EXE)

.PHONY: clean 
