# Can't use alpine because the race detector needs glibc and alpine doesn't seem
# to have a glibc we can use easily.
FROM golang:1.6.3

RUN apt-get update && apt-get install -y --no-install-recommends \
		g++ \
		gcc \
		libc6-dev \
		make \
	&& rm -rf /var/lib/apt/lists/*

COPY . /go/src/github.com/jmhodges/lekube/

RUN go install github.com/jmhodges/lekube/lekube-fetch && \
    rm -rf /go/src
CMD ["lekube-fetch", "-conf", "/etc/lekube/lekube.json", "-prod"]
