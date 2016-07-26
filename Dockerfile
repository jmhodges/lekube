FROM golang:1.6.3-alpine

# glibc is required to build with the race detector
RUN apk add --no-cache glibc-dev

RUN mkdir -p /etc/lekube-fetch

COPY . /go/src/github.com/jmhodges/lekube/

RUN apk add --no-cache gcc && \
   go install -race github.com/jmhodges/lekube/lekube-fetch && \
   rm -rf /go/src/ && \
   apk del gcc

CMD ["lekube-fetch", "-conf", "/etc/lekube/lekube.json"]
