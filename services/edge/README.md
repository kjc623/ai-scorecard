# edge

The device entry point where a deployment has no Application Gateway: pre-prod on Fly.io
(`fly/edge/fly.toml`) and the local lab. It receives the device's TCP connection untouched,
terminates TLS itself, and forwards the device API to ingest-api and control-api as the gateway
does (`azure/modules/application-gateway.bicep`).

## How it works

- **TLS.** The server certificate is the device hostname's, from a public CA. A client certificate
  is requested but neither required nor verified: a device's first enrolment presents none, and
  ingest-api and control-api verify the ones that are presented against the device CA.
- **Allow-list.** Only the device API is reachable: `/v1/events` goes to ingest-api; `/v1/enrol`,
  `/v1/policy`, `/v1/health`, `/v1/content/grant`, `/v1/content` and `/v1/extension/*` go to
  control-api. Any other path is refused with 403; an allowed prefix that no route serves gets 502.
- **Forwarding.** The presented leaf certificate travels URL-encoded in `X-Client-Cert`, with
  `X-Forwarded-Proto: https` and `X-Forwarded-Host`. A copy of any of them sent by the client is
  discarded, so only the edge can supply a certificate.
- **Limits.** 10 s to read the headers, 60 s to read a request and 60 s to write its answer, 90 s
  idle keep-alive, 32 KiB of headers.

It has no health route of its own: a path outside the allow-list is refused, so the platform checks
its TCP port. Logs are JSON on stdout.

## Configuration

| Variable | Meaning |
|---|---|
| `EDGE_TLS_CERT_PEM`, `EDGE_TLS_KEY_PEM` | Required. The device hostname's certificate chain and private key, PEM |
| `EDGE_INGEST_URL` | ingest-api's address, default `http://ingest-api:8080` |
| `EDGE_CONTROL_URL` | control-api's address, default `http://control-api:8080` |
| `EDGE_ADDR` | Listen address, default `0.0.0.0:8443` |

## Build and test

```sh
docker build -f services/edge/Dockerfile -t edge .   # from the repository root
cd services/edge
gofmt -l . && go vet ./... && go test ./...
```
