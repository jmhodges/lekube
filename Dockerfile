# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.25.7@sha256:cc737435e2742bd6da3b7d575623968683609a3d2e0695f9d85bee84071c08e6 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian12
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]
