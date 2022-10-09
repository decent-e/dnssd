FROM golang:latest as builder

COPY . /app/
WORKDIR /app
RUN mkdir -p /app/root
RUN mkdir -p /app/lib64 
RUN mkdir -p /tmp
RUN mkdir -p /app/bin
WORKDIR /app/bin
RUN CGO_ENABLED=0 go build ../cmd/relay/.
RUN CGO_ENABLED=0 go build ../cmd/debug/.
RUN CGO_ENABLED=0 go build ../cmd/register/.
RUN CGO_ENABLED=0 go build ../cmd/browse/.
RUN CGO_ENABLED=0 go build ../cmd/resolve/.
RUN CGO_ENABLED=0 go build ../cmd/bct/.

FROM scratch

COPY --from=builder /app/bin/relay /bin/relay
COPY --from=builder /app/bin/debug /bin/debug
COPY --from=builder /app/bin/register /bin/register
COPY --from=builder /app/bin/resolve /bin/resolve
COPY --from=builder /app/bin/browse /bin/browse
COPY --from=builder /app/bin/bct /bin/bct
COPY --from=builder /app/lib64 /tmp
COPY --from=builder /app/lib64 /lib64
COPY --from=builder /app/root /root

# Command to run
CMD ["/bin/relay", "-mode=client"]
