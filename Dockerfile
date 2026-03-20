FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY ghost /usr/local/bin/ghost
ENTRYPOINT ["ghost"]
CMD ["serve"]
