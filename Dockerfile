# Can't use alpine because the race detector needs glibc and alpine doesn't seem
# to have a glibc we can use easily.
FROM golang:1.14.1

COPY ./lekube /go/bin/

CMD ["lekube", "-conf", "/etc/lekube/lekube.json"]
