# llmesh

A lightweight, self-hosted LLM router that pools your llama.cpp instances into a single API endpoint.

## Quick Start

### Docker

```bash
# Router (server)
docker compose up -d

# Client (local machine)
cp config.yaml.example config.yaml
# Edit router_url, router_token, models
docker compose -f docker-compose.client.yml up -d
```

### Build from source

```bash
git clone <repo> llmesh && cd llmesh
# Build both images
docker compose build
docker compose -f docker-compose.client.yml build
```

## Structure

```
llmesh/
├── router/          # Go router server
├── client/          # Go client binary
├── docker-compose.yml       # Router deployment
└── docker-compose.client.yml # Client deployment
```

## Links

Replace `[HOST]` with the server's IP or domain, and `[PORT]` with the port from `config.yaml` (default: 53002).

- **Admin Dashboard**: http://[HOST]:[PORT]/admin
- **OpenAI API**: http://[HOST]:[PORT]/v1/chat/completions
- **Anthropic API**: http://[HOST]:[PORT]/v1/messages

## License

Private / self-hosted only.
