# Config

Configuration is loaded via Viper from `config.yaml` and environment variables
prefixed with `ABE_`. The loader tolerates a missing config file (env-only).

Validation enforces required fields and consistency:
`server.port`, `workers.count`, `batching.queue_capacity`,
`batching.min_batch_size`, and `batching.max_batch_size`/`min_wait_ms` ordering.
