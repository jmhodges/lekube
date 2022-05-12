# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.18.2-buster@sha256:949a195c5d44e7b7f71499c146def008c7e98b8d02c276e567ab2e5c6971587b as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian10@sha256:37400e73e324f00d053db6e5e375b4176f9498d0dfdbd05089a06714d71b86f0
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]