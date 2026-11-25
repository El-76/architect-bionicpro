FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bionicpro-reports .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 app
COPY --from=build /out/bionicpro-reports /usr/local/bin/bionicpro-reports
USER 10001
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/bionicpro-reports"]
