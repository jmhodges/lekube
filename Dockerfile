FROM golang:1.6.2-alpine

RUN mkdir -p /etc/lekube-fetch

# FIXME instead of this, build lekube outside the dockerfile
RUN apk add build-base

COPY . /go/src/github.com/jmhodges/lekube/

RUN go install -race github.com/jmhodges/lekube/lekube-fetch && \
    rm -rf /go/src/

CMD ["lekube-fetch", "-conf", "/etc/lekube/lekube.json"]
