// Package e2e holds the backend's end-to-end tests. They build the real
// server (cmd/server), start it against a real Postgres and S3 and talk to it
// the way browsers and agents do: plain HTTP with the identity cookie,
// WebSockets for live sessions and /mcp-driven tabs, presigned URLs straight
// to S3, and JSON-RPC on /mcp. Nothing inside the server is replaced.
//
// They need TEST_DATABASE_URL and S3_TEST_ENDPOINT (+ _ACCESS_KEY,
// _SECRET_KEY, _BUCKET; S3_TEST_SSL and S3_TEST_REGION for a real
// provider) and are skipped without them. run.sh starts throwaway services
// in Docker and runs them.
package e2e
