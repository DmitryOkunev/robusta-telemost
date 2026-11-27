# Robusta Telemost

A Go service for sending enriched Robusta Classic Prometheus alerts to Yandex Telemost. It consumes the stock JSON webhook sink. A firing alert opens a conversation with a caption, available PNG graphs and a reaction on the parent. A resolved alert posts in that thread and edits the original caption to ✅ resolved and changes the parent's reaction.

## Connect Robusta

1. Run one service replica and a persistent Valkey Cluster. A single shard can serve the initial workload. Each bot must belong to its destination chats and have permission to publish.
2. Set the variables below. Inject credentials from your secret manager. Expose the service to the runner on a private endpoint. Use an authenticated, encrypted transport when crossing network trust boundaries.
3. Merge the example sink into your Robusta values and assign it to the playbooks that produce the alert's enrichments. Retain the allowlist for each project and environment. Switch each destination over once to avoid a second delivery through an existing Messenger sink. [Example Helm values](examples/robusta-values.yaml)

| Service variable | Value |
| --- | --- |
| `TELEMOST_ROUTES` | JSON map, for example `{"example-dev":"<dev-chat-id>","example-prod":"<prod-chat-id>"}`. The webhook path selects the route. |
| `WEBHOOK_TOKEN` | Shared secret. Put the full `Bearer <secret>` value in the runner's authorization secret. Robusta 0.55.0 can log the outbound authorization header when a webhook fails. Use a dedicated credential and restrict access to runner logs. |
| `YAMESSENGER_BOT_TOKEN` | OAuth bot token. Only the Go service needs it. |
| `VALKEY_ADDRS` | Comma-separated Valkey Cluster bootstrap addresses with ports. The service must also reach addresses announced by nodes. |
| `VALKEY_USERNAME`, `VALKEY_PASSWORD` | ACL credentials for the service. The tested grant is shown below. |
| `TELEMOST_PROJECT_LABELS` | Optional JSON map, e.g. `{"example-desk":"💼 Example Desk"}`. A finding with `observability_profile=example-desk-test` gets the prefix `💼 Example Desk TEST` on its firing and resolved messages. Unmapped profiles keep the standard heading. |
| `FIRING_REACTION`, `RESOLVED_REACTION` | Names accepted by the chat, defaulting to `fire` and `like`. Check the chat's available reactions. |
| `LISTEN_ADDR` | Defaults to `:8080`. Liveness is `GET /healthz` and Valkey readiness is `GET /readyz`. |

Resolved replies and edited parent captions include the start, end and elapsed time reported by Alertmanager. Set `INCIDENT_TIMEZONE` to an IANA zone (default UTC). Missing or invalid end times are displayed as unavailable.

Optional `INCIDENT_METRICS` JSON maps `profiles` to trusted VictoriaMetrics URLs and `signals` to the measured series and its worst direction. For example:

```json
{"profiles":{"example-dev":{"namespace":"example-dev","cluster":"example","route":"dev","url":"http://vmselect:8481/select/10/prometheus"}},"signals":{"MemoryHigh":{"series":"Memory, %","worst":"max"}}}
```

The rule's Grafana annotation `dashboard_graph_url` supplies `var-expression`. Its query must return the configured series in the `job` label, tagged with namespace and object labels. An optional `Порог …` series provides the threshold. The service queries the configured tenant at the event end, including both incident boundaries and one-minute samples across the interval. A missing closing sample is shown separately. Lookup failures still allow notification delivery.

Scrape `GET /metrics` for `telemost_delivery_stage_seconds` (stages `webhook_to_publish`, `publish_to_reaction`; events `firing`, `resolved`), `telemost_bot_api_requests_total` (method and result), and `telemost_summary_reads_total`. The reaction timer starts at acknowledgement of the root or thread reply, including subsequent edits and retries. It measures Bot API acknowledgement.

Use a dedicated Valkey user with key pattern `~telemost:*` and commands `+ping +cluster|slots +cluster|shards +get +set +del +eval`. The service uses a single-key compare-and-set Lua script to claim each outbound post. Configure AOF and a persistent volume for state that must survive a Valkey restart. The current client uses plaintext TCP to Valkey inside the trusted network.

## Delivery behavior

The service accepts opted-in Prometheus findings with the label `robusta=enabled` and groups them by destination chat, source cluster label, fingerprint and normalized start time. A new occurrence of the same alert gets its own thread. Robusta 0.55.0 prefixes resolved titles with `[RESOLVED] `. Its `failure` flag can be true for both states.

PNG graphs are rendered from the finite samples in Robusta's matrix enrichments. Text blocks carry log links from existing playbooks. Tenant selection stays with those playbooks. Findings with no plottable samples use a text message. The PNG uses a dark background, UTC time labels, a color legend and a marker at the last observed sample. Missing values break the line. Robusta-specific axis formats, threshold overlays and non-graph file attachments are outside this first version.

The webhook responds with HTTP 204 after delivery or a recognized repeat. While an incident is open, Valkey stores the caption for resolution and the references to its original images. Editing the gallery reuses those references even if the resolved event has no graph. Retrying an edit or reaction reuses the parent without posting another reply. Incidents opened with v0.1.0 retain their original captions because their records contain only a message ID. An open thread stays in Valkey until resolution. Closed incidents are remembered for 30 days.

If a post times out, or the process stops between claiming the post and saving its ID, the state remains pending. A replay receives HTTP 409 to prevent a second top-level post. An orphan resolution also receives 409. A bot rejection with an explicit 4xx error permits a replay. Robusta 0.55.0 logs failed webhook calls and has no durable retry queue. Replaying failed deliveries and reconciling uncertain posts currently require operator intervention.

To reconcile, locate the specific `telemost:incident:<digest>` key from the service error log and inspect the chat and the key. Pause deliveries for that incident before editing state. For a confirmed published root, set the phase to `root_sent` with its integer `Parent` message ID and replay. For a confirmed published reply, use `reply_sent` and the original parent ID. If the API definitely rejected the root, remove its reservation. For a definitely rejected reply, restore `open` with the existing parent. A missing item in an incomplete chat history is insufficient evidence to resend.

## Development

```sh
go vet ./...
go test ./...
./scripts/test-cluster.sh
docker build -t robusta-telemost:dev .
```

The integration script starts a disposable Valkey Cluster and uses a local HTTP server for the Telemost API. Tests cover gallery and text requests, per-channel state, a fresh service instance reading the same Valkey, duplicate deliveries, partial failures and ambiguous responses. The examples contain synthetic destinations.

[Robusta webhook documentation](https://docs.robusta.dev/master/configuration/sinks/webhook.html) · [Telemost galleries](https://yandex.ru/dev/messenger/doc/ru/api-requests/message-send-gallery) · [Telemost reactions](https://yandex.ru/dev/messenger/doc/ru/api-requests/message-send-reaction)

## Optional HolmesGPT thread analysis

After a new firing card and its reaction are confirmed, the service can ask a standalone HolmesGPT `POST /api/chat` for a short analysis and post it in that thread. The model and provider credentials belong to Holmes. Enable the worker with `HOLMES_CONFIG={"enabled":true,"endpoint":"http://holmes","contextLabels":["service","deployment","reason"]}`. Mount a YAML file and set `OBSERVABILITY_PROFILES_FILE` to its full path. Its keys match the alert label `observability_profile`.

```yaml
profiles:
  example-dev:
    route: dev
    cluster: sandbox
    namespace: example-dev
    metrics:
      url: https://metrics.example.invalid/select/10/prometheus
      auth: {bearerTokenEnv: EXAMPLE_METRICS_TOKEN}
    logs:
      url: https://logs.example.invalid
      accountID: "1"
      projectID: "0"
      auth: {usernameEnv: EXAMPLE_LOG_USER, passwordEnv: EXAMPLE_LOG_PASSWORD}
```

Both sources support optional Bearer or Basic authentication. Populate the named environment variables from Secret keys. Omit `auth` for an internal endpoint without credentials. The two log IDs can be omitted together for a single-tenant or upstream-authenticated endpoint. Keep signal definitions in `INCIDENT_METRICS.signals`. Existing deployments can continue using their inline profiles for counts and summaries.

Every routed firing alert in a configured profile qualifies, including alerts without a graph. The route, cluster and namespace must match. The payload includes bounded title and description from the finding, severity, times and selected object labels. Use `contextLabels` to include additional alert and series dimensions. Available graphs contribute up to eight visible matching series with minimum, maximum and last values; configured percentage signals retain their measurement and threshold. The payload also includes 15-minute VictoriaLogs counts (total, warning, error). Logs are scoped to the container or pod when present, otherwise to the namespace. The service omits raw log messages, other annotations and unlisted labels; it replaces HTTP URLs in alert text with a placeholder. Alert authors should keep descriptions and selected label values suitable for the model provider. A failed log lookup leaves the counts unavailable.

For interactive investigation, configure the native Holmes HTTP toolset with the sink host, method `POST`, and paths `/v1/observability/logs`, `/v1/observability/names`, and `/v1/observability/metrics`. Set its `config.extra_headers.X-Holmes-Query-Key` to `{{ request_context.headers['X-Holmes-Query-Key'] }}`. Each operation accepts a JSON object with a single `query` string for a LogsQL row filter, metric selector or MetricsQL expression respectively. The service issues a short-lived key for each investigation, selects the configured sources, and adds cluster and namespace filters on the backend. Raw log excerpts and metric results returned by these tools enter the model context.

Analysis is best effort. One background worker processes up to 128 waiting jobs. A restart loses outstanding analysis jobs; ordinary alert and resolution delivery keep their existing Valkey semantics. The worker checks incident state before querying Holmes and again before posting. Check `telemost_holmes_investigations_total` for queue overflow, lookup failures and delivery outcomes. An empty configuration or `{"enabled":false}` disables analysis.
