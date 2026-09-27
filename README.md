# Traefik Subdomain Path Rewrite Plugin

This plugin for Traefik enables dynamic path rewriting based on subdomains and provides fallback capabilities for handling 404 responses. It's particularly useful for scenarios where you want to map subdomain-based URLs to path-based routes while maintaining flexibility in URL structure.

## Features

- Rewrite subdomain-based URLs to path-based routes
- Optional path preservation after rewriting
- Configurable base path for all rewrites
- Custom host replacement
- Fallback document for page navigations the backend cannot answer (single-page apps)
- Responses stream through; only a response that is replaced by the fallback is held back
- Detailed request tracking through custom headers

## Configuration

### Static Configuration

Static configuration is defined in your Traefik installation and enables the plugin. This is typically done in your `traefik.yml` file or through environment variables.

```yaml
experimental:
  plugins:
    subdomainPathRewrite:
      moduleName: "github.com/lukas-r/traefik-subdomain-path-rewrite-plugin"
      version: "v0.4.0"
```

### Dynamic Configuration

Dynamic configuration can be modified at runtime and controls the plugin's behavior for specific routers.

#### Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| rewriteSubdomain | bool | true | Enable/disable subdomain extraction and rewriting |
| replacementHost | string | "" | Override the target host. If empty, uses the base domain |
| basePath | string | "" | Base path prefix for all rewritten URLs |
| keepPath | bool | true | Preserve the original path after the rewritten portion |
| fallbackPath | string | "" | Document to serve when a page navigation gets a fallback status |
| fallbackStatusCodes | []int | [404] | Backend statuses that trigger the fallback |
| logLevel | string | "INFO" | Logging level (INFO, DEBUG, ERROR) |

#### Detailed Parameter Behavior

##### rewriteSubdomain

- When `true`: Extracts the first subdomain segment and uses it in the path rewrite
- When `false`: No subdomain extraction occurs, other rewrite rules still apply
- Example: With `true`, `customer.example.com` becomes `example.com/customer`
- Subdomain extraction uses regex to capture everything before the first dot

##### replacementHost

- If set: Replaces the entire host with this value
- If empty: Uses the original host minus the extracted subdomain
- Example: With `replacementHost: "api.internal"`, `customer.example.com` becomes `api.internal`
- Useful for routing to internal services or different domains

##### basePath

- Prefixed to all rewritten paths
- If provided without leading slash, one is automatically added
- Applies before the subdomain segment in the final path
- Example: With `basePath: "/api"`, the path becomes `/api/customer/...`

##### keepPath

- When `true`: Preserves the original request path after the rewritten portion
- When `false`: Only uses the rewritten portion, ending with a slash
- Example with `true`: `/original/path` becomes `/api/customer/original/path`
- Example with `false`: `/original/path` becomes `/api/customer/`

##### fallbackPath

The fallbackPath parameter has two distinct behaviors based on whether it starts with a slash:

1. Absolute Path (starts with slash):
   - Appended to the base rewritten path
   - Original path is completely ignored
   - Example:
     - `fallbackPath: "/default"`
     - Original: `customer.example.com/not/found`
     - Fallback: `example.com/api/customer/default`

2. Relative Path (no starting slash):
   - Replaces only the last segment of the original path
   - Preserves the rest of the path structure
   - Example:
     - `fallbackPath: "default"`
     - Original: `customer.example.com/products/not-found`
     - Fallback: `example.com/api/customer/products/default`

##### Which requests fall back

The fallback is served in place of the backend's response only when all of these hold:

- the backend answered with one of `fallbackStatusCodes`;
- the request is `GET` or `HEAD`;
- it is a page navigation:
  - with browser fetch metadata: `Sec-Fetch-Mode: navigate`;
  - without it: `Accept` lists `text/html` or `application/xhtml+xml` (with a non-zero quality), or the last path segment has no file extension and `Accept` is empty or accepts `*/*`.

A request for a file that does not exist (`/assets/missing.js`) or an API call (`Accept: application/json`) keeps the backend's status, so a broken build shows up as a 404 instead of an HTML page. The fallback request is dispatched to the same backend inside Traefik, so the fallback document does not need a publicly resolvable host.

##### fallbackStatusCodes

- Defaults to `[404]`
- Add `403` for backends that answer missing keys with Access Denied, for example an S3 bucket without list permission

##### logLevel

- "INFO": Standard operational logging (per-request details are DEBUG only)
- "DEBUG": Detailed request/response information
- "ERROR": Only error conditions
- Affects the verbosity of plugin logs

### Example Configuration (Docker Labels)

```yaml
services:
  my-service:
    labels:
      - "traefik.enable=true"
      # Enable the plugin for this router
      - "traefik.http.routers.my-service.middlewares=subdomain-rewrite"
      - "traefik.http.middlewares.subdomain-rewrite.plugin.subdomainPathRewrite.rewriteSubdomain=true"
      - "traefik.http.middlewares.subdomain-rewrite.plugin.subdomainPathRewrite.basePath=/api"
      - "traefik.http.middlewares.subdomain-rewrite.plugin.subdomainPathRewrite.keepPath=true"
      - "traefik.http.middlewares.subdomain-rewrite.plugin.subdomainPathRewrite.fallbackPath=/default"
```

## URL Rewriting Examples

Based on the example configuration above, here's how different URLs would be rewritten:

| Original URL | Rewritten URL | Notes |
|-------------|---------------|--------|
| `customer1.example.com/users` | `example.com/api/customer1/users` | Subdomain becomes path segment |
| `customer2.example.com/` | `example.com/api/customer2/` | Minimal path case |
| `customer3.example.com/orders/123` | `example.com/api/customer3/orders/123` | Complex path preservation |
| `customer4.example.com/products/not-found` | `example.com/api/customer4/default` | Absolute fallback path (`/default`), page navigation |
| `customer5.example.com/catalog/missing` | `example.com/api/customer5/catalog/default` | Relative fallback path (`default`), page navigation |
| `customer6.example.com/assets/missing.js` | `example.com/api/customer6/assets/missing.js` | File request: no fallback, the 404 is returned |

## Headers

The plugin sets these headers on the request it forwards. Values a client sends for them are removed first, so they cannot be used to skip the rewrite:

- `X-Replaced-Path`: Original path before rewriting
- `X-Replaced-Host`: Original host before rewriting
- `X-Fallback-For`: The rewritten path the fallback replaces (set on the fallback request to the backend)

These headers are useful for debugging and understanding how requests are being transformed by the plugin.

## Development

Traefik runs plugins in the [Yaegi](https://github.com/traefik/yaegi) interpreter, so run the tests under both Go and Yaegi:

```bash
go test ./...
yaegi test -v .
```

Under Yaegi, a response writer the plugin hands to Traefik is wrapped without `http.Flusher`, so explicit flushes from the backend are not propagated. Writes still pass straight through and are sent as the server's write buffer fills.
