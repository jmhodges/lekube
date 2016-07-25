FROM golang

# :1.6.2-alpine

RUN mkdir -p /etc/lekube-fetch

COPY . /go/src/github.com/jmhodges/lekube/

RUN go install -race github.com/jmhodges/lekube/lekube-fetch && \
    rm -rf /go/src/

CMD ["lekube-fetch", "-conf", "/etc/lekube/lekube.json"]
