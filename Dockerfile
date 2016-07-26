FROM golang:1.6.3-alpine

RUN mkdir -p /etc/lekube-fetch

COPY . /go/src/github.com/jmhodges/lekube/

RUN apk add --no-cache --virtual build-deps gcc musl-dev && \
   go install -race github.com/jmhodges/lekube/lekube-fetch && \
   rm -rf /go/src/ && \
   apk del .build-deps

CMD ["lekube-fetch", "-conf", "/etc/lekube/lekube.json"]
