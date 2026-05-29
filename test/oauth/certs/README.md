# Test-Only Self-Signed Certificates

These are test-only certificates for the local Keycloak container. **Do not reuse in other fixtures or environments.**

To regenerate:

```sh
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout keycloak.key -out keycloak.crt \
  -days 3650 -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
```
