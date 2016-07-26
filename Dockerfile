FROM golang:1.6.3-alpine

RUN mkdir -p /etc/lekube-fetch

COPY . /go/src/github.com/jmhodges/lekube

RUN apk add --no-cache gcc musl-dev
RUN go install -race github.com/jmhodges/lekube && rm -rf /go/src/

CMD ["lekube-fetch", "-conf", "/etc/lekube/lekube.json"]
