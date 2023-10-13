# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM amd64/golang:1.21.3@sha256:d0214956a9c50c300e430c1f6c0a820007ace238e5242c53762e61b344659e05 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/static-debian12@sha256:0c3d36f317d6335831765546ece49b60ad35933250dc14f43f0fd1402450532e
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]
