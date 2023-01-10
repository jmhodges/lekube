# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.19.4-buster@sha256:7c97baef6d7a48c2520691e38cca1c56a86b0cad36831c01d959913711f30aa3 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian10@sha256:d8244d4756b5dc43f2c198bf4e37e6f8a017f13fdd7f6f64ec7ac7228d3b191e
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]