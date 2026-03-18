.PHONY: build run clean

build:
	go build -o claudeprof .

run: build
	./claudeprof

clean:
	rm -f claudeprof
