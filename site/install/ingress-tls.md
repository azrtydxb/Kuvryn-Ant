# Ingress and TLS

Package clients, the console and the API all use one HTTPS origin, for example `https://ant.example.com`. Kuvryn Ant listens on plain HTTP port 8080 inside the cluster; TLS is terminated by your ingress controller or Gateway. The optional federation listener is the exception: it terminates mTLS in the pod.

## Public URL

`publicURL` (env `ANT_PUBLIC_URL`) is the origin clients use. It must be `https://host[:port]` with no path. Ant uses it to build every absolute URL it returns (npm tarball links, Terraform download URLs, login callbacks and so on). It never derives URLs from the `Host` header or from `X-Forwarded-*` headers.

- The chart requires `ingress.host` to equal the host of `publicURL`, and `httpRoute.hostnames` to include it.
- Browser identity providers must use the same origin (`/auth/oidc/callback`, `/auth/saml/callback`).
- Ant must be served at the root of the host. Path prefixes such as `https://example.com/ant/` are not supported.

## Routed paths

The chart's Ingress and HTTPRoute send these paths to the Service:

| Path       | Match  | Purpose                                                    |
| ---------- | ------ | ---------------------------------------------------------- |
| `/`        | Exact  | Redirects to `/console/`                                   |
| `/console` | Prefix | Web console                                                |
| `/api`     | Prefix | Management API (`/api/v1`)                                 |
| `/auth`    | Prefix | Browser login, sessions, SAML metadata                     |
| `/v2`      | Prefix | OCI distribution API                                       |
| `/feeds`   | Prefix | All other package formats (`/feeds/<slug>/<format>/...`)   |
| `/mcp`     | Prefix | Model Context Protocol endpoint (service tokens only)      |
| `/hook`    | Prefix | Incoming webhooks (`/hook/<slug>`), authenticated per hook |

Not routed: `/health/live`, `/health/ready` and `/metrics` (keep them cluster-internal; scrape metrics through `metrics.headlessService`).

## ingress-nginx

The chart creates a plain `networking.k8s.io/v1` Ingress and **sets no controller annotations**. Controller defaults are unsuitable for an artifact server: ingress-nginx limits request bodies to 1 MiB, buffers request bodies and times out reads after 60 seconds. Set the limits explicitly through `ingress.annotations`:

```yaml
publicURL: https://ant.example.com
ingress:
  enabled: true
  className: nginx
  host: ant.example.com
  tlsSecret: ant-tls
  annotations:
    # No request-size limit (image layers, model files, large archives).
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    # Stream uploads to Ant instead of spooling them in the controller.
    nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
    # Stream downloads instead of buffering them in the controller.
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    # Long transfers and slow pull-through fetches.
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

Ant itself cuts a transfer only when it makes no progress for 60 seconds, and extends the write deadline while it waits for a remote upstream. Proxy timeouts shorter than your largest transfer break large uploads and downloads with `504` or truncated bodies.

## Gateway API

With a Gateway API implementation (Envoy Gateway, Istio, Cilium, and others), enable `httpRoute` instead of `ingress`. TLS terminates on the Gateway listener you reference.

```yaml
publicURL: https://ant.example.com
httpRoute:
  enabled: true
  parentRefs:
    - name: edge
      namespace: gateway-system
      sectionName: https
  hostnames:
    - ant.example.com
  timeouts:
    request: "0s" # "0s" disables the timeout where the implementation supports it
    backendRequest: "0s"
```

`timeouts.request` and `timeouts.backendRequest` map to the standard `HTTPRoute` rule timeouts. Request-body size and buffering limits are implementation-specific (for example an Envoy Gateway `ClientTrafficPolicy` or `BackendTrafficPolicy`) and are not set by the chart.

## Certificates with cert-manager

### Ingress

cert-manager's ingress integration issues the certificate into the Secret named by `ingress.tlsSecret`:

```yaml
ingress:
  enabled: true
  className: nginx
  host: ant.example.com
  tlsSecret: ant-tls
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

`letsencrypt-prod` stands for a `ClusterIssuer` you have already created.

### Gateway

For a Gateway, issue the certificate for the listener with a `Certificate` resource (or cert-manager's Gateway API integration) in the Gateway's namespace:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ant-example-com
  namespace: gateway-system
spec:
  secretName: ant-example-com-tls
  dnsNames:
    - ant.example.com
  issuerRef:
    kind: ClusterIssuer
    name: letsencrypt-prod
```

Reference `ant-example-com-tls` in the Gateway listener's `tls.certificateRefs`.

### Private CA

If you use an internal CA, every client must trust it: Docker (`/etc/docker/certs.d/ant.example.com/ca.crt`), containerd, npm (`cafile`), pip (`cert`), and so on. See [CA trust](../host-caching/ca-trust.md).

## Trusted proxies

Behind a proxy every request arrives from the proxy's address. Ant uses the client address for authentication throttling, per-client limits, anonymous-access network groups and audit records. List the proxy pod network so that the forwarded client address is believed:

```yaml
trustedProxies:
  - 10.0.0.0/16 # pod CIDR of the ingress controller or gateway pods
```

This sets `ANT_TRUSTED_PROXIES`. Only when the TCP peer is inside one of these ranges does Ant read `X-Forwarded-For` (and `X-Real-IP`), walking the chain from the right and skipping trusted hops, so a client cannot choose its own address by sending the header. With an empty list no forwarding header is trusted. Keep the list as narrow as possible: anything listed can claim arbitrary client addresses.

If your load balancer does not preserve client IPs (for example a TCP load balancer in front of the controller without PROXY protocol), the controller itself only sees the load balancer address; fix this at the load balancer or controller level.

## Access logs

Some Ant URLs carry short-lived signatures in the query string (for example signed Terraform downloads and federation blob URLs). Ant redacts them in its own logs, but your controller logs the full request URI by default. Configure the controller to log the path without the query string, for example `$uri` instead of `$request_uri` in an ingress-nginx `log-format-upstream`, or `%REQ_WITHOUT_QUERY(X-ENVOY-ORIGINAL-PATH?:PATH)%` in an Envoy Gateway access-log format.

## Health and metrics

- `/health/live` (process) and `/health/ready` (PostgreSQL reachable within 2 seconds) are used by the kubelet probes on the pod port directly.
- `/metrics` serves Prometheus metrics on the same port. Enable `metrics.headlessService` for scraping and restrict it with `networkPolicy.ingressFrom`. Do not expose it through the public ingress.

## Federation listener

Federation peers connect to a separate mTLS listener (`federation.port`, default 8444) whose TLS must reach the pod **unterminated**. Use the federation Service directly (`LoadBalancer` or `NodePort`), `federation.ingressPassthrough` with ingress-nginx started with `--enable-ssl-passthrough`, or `federation.tlsRoute` on a Gateway listener in TLS passthrough mode. Never terminate federation TLS at a proxy. See [Federation](../replication/federation.md).
