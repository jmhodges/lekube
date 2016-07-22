FROM golang

# :1.6.2-alpine

RUN mkdir -p /etc/lekube-fetch

COPY . /go/src/github.com/jmhodges/lekube/

RUN cp /go/src/github.com/jmhodges/lekube/testdata/test.json /etc/lekube-fetch/ && \
    go install github.com/jmhodges/lekube/lekube-fetch && \
    rm -rf /go/src/

CMD ["lekube-fetch", "-conf", "/etc/lekube-fetch/test.json", "-betweenChecksDur", "1m"]
