FROM golang:1.6.3-alpine

RUN mkdir -p /etc/lekube-fetch

COPY lekube-fetch/lekube-fetch /go/bin/

CMD ["lekube-fetch", "-conf", "/etc/lekube/lekube.json"]
