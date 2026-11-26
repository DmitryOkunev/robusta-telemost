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
