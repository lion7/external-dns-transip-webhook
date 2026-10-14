# ExternalDNS webhook provider for TransIP

[ExternalDNS](https://github.com/kubernetes-sigs/external-dns) removed its TransIP provider in
v0.22.0. This is that provider (taken from v0.21.0) as a
[webhook provider](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/):
a sidecar of ExternalDNS that holds the TransIP credentials and does the API calls.

## What changed compared with the in-tree provider

- **One endpoint per record set.** All entries with the same name and type are returned as one
  endpoint with several targets. The in-tree provider returned one endpoint per entry, and the
  planner only saw the last one.
- **Creating an entry that exists is not an error.** TransIP answers "this exact record already
  exists"; the in-tree provider returned that, and ExternalDNS exited on it.
- **Updates and deletes are precise.** An update adds the missing entries first and then removes
  the obsolete ones (a CNAME the other way around), so a record set keeps answering. A delete
  removes only the targets of the endpoint. The in-tree provider removed every entry with that
  name and type in both cases.
- **No set identifiers.** TransIP cannot store one, so an endpoint that has one never matched its
  own record and was created again on every sync. They are dropped in `AdjustEndpoints`.
- **API calls time out.** The in-tree provider used Go's default HTTP client, which never gives
  up. When the TransIP API did not answer, `/records` outlasted the 15 seconds ExternalDNS waits
  for its webhook (`--webhook-provider-read-timeout` plus `--webhook-provider-write-timeout`), and
  ExternalDNS exited. Now every call gives up after `--transip-timeout`, and the error is retried
  like any other (below).
- **API errors are retried.** Every error becomes an HTTP 500, which ExternalDNS treats as a soft
  error: it logs it and tries again on the next sync, where the in-tree provider made it exit.

A name and type is owned as a whole: an update replaces all entries of that record set, also the
ones that were added by hand.

## Configuration

Every flag has an environment variable as its default.

| Flag | Environment | Default | Meaning |
|------|-------------|---------|---------|
| `--transip-account` | `TRANSIP_ACCOUNT` | | TransIP account name |
| `--transip-keyfile` | `TRANSIP_KEYFILE` | | path to the private key of the account |
| `--transip-timeout` | `TRANSIP_TIMEOUT` | `5s` | give up on a TransIP API call after this long |
| `--domain-filter` | `DOMAIN_FILTER` | all zones | zone to manage, can be repeated |
| `--dry-run` | `DRY_RUN` | `false` | log the changes, apply none |
| `--listen-address` | `LISTEN_ADDRESS` | `127.0.0.1:8888` | webhook API |
| `--health-address` | `HEALTH_ADDRESS` | `:8080` | `/healthz` |
| `--log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warning`, `error` |
| `--log-format` | `LOG_FORMAT` | `text` | `text` or `json` |

`DOMAIN_FILTER` is a comma separated list. The webhook API has no authentication, keep it on
localhost.

### Dry run

`--dry-run` of ExternalDNS does **not** reach a webhook provider: ExternalDNS still sends the
changes, and applying them or not is up to the provider. Set `--dry-run` on this sidecar instead.
It then also uses the TransIP API in read-only mode, and logs every entry it would add or remove.

## Deployment

With the ExternalDNS Helm chart, and the private key in the Secret `transip-credentials` under the
key `privatekey`:

```yaml
provider:
  name: webhook
  webhook:
    image:
      repository: ghcr.io/lion7/external-dns-transip-webhook
      tag: 0.1.0
    args:
      - --transip-account=example
      - --transip-keyfile=/transip/privatekey
      - --domain-filter=example.org
    extraVolumeMounts:
      - name: transip-credentials
        mountPath: /transip
        readOnly: true
extraVolumes:
  - name: transip-credentials
    secret:
      secretName: transip-credentials
```

The chart's probes on `/healthz` (port 8080) work as they are. Applying a change is one TransIP API
call per entry; when a sync changes more than a few dozen entries, ExternalDNS may give up waiting
(`--webhook-provider-write-timeout`, 10 seconds by default). The sidecar finishes the changes
anyway, and the next sync finds nothing left to do.

## ACME DNS-01 challenges

With [cert-manager-webhook-external-dns](https://github.com/lion7/cert-manager-webhook-external-dns)
cert-manager presents its challenges as `DNSEndpoint` objects, so ExternalDNS stays the only
component with DNS credentials. Run ExternalDNS with `--policy=sync` to have the challenge record
and its ownership record removed after validation; with `--policy=upsert-only` ExternalDNS never
sends a delete and they stay behind.

## Development

```bash
go test ./...
ko build --local .
```

The tests in `internal/transip` run the provider under the real ExternalDNS planner and TXT
registry against an in-memory TransIP repository.

## License

Apache License 2.0, as ExternalDNS, which `internal/transip` is derived from.
