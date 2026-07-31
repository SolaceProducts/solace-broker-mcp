# Security Policy

## Supported Versions

We release patches for security vulnerabilities in the following versions:

| Version | Supported          |
| ------- | ------------------ |
| latest (main branch) | :white_check_mark: |
| < 1.0   | :x: (pre-release)  |

Once we reach v1.0, we will maintain security updates for the current major version and the previous major version.

## Reporting a Vulnerability

**Do not report security vulnerabilities via public GitHub issues.**

We take security seriously. If you discover a security vulnerability in the Solace Broker MCP Server, please report it responsibly.

### How to Report

**Email**: [andrea.ross@solace.com](mailto:andrea.ross@solace.com)

Please include the following in your report:

- **Description** of the vulnerability
- **Steps to reproduce** the issue
- **Potential impact** (what an attacker could achieve)
- **Affected versions** (if known)
- **Suggested fix** (if you have one)
- **Your contact information** for follow-up questions

### What to Expect

When you report a security vulnerability, here's what you can expect from us:

**Initial Response**: Within **48 hours** (business days), we will acknowledge receipt of your report.

**Assessment**: Within **1 week**, we will provide an initial assessment of the vulnerability including:
- Confirmation that we can reproduce the issue
- Severity classification (Critical, High, Medium, Low)
- Expected timeline for a fix

**Fix Timeline**:
- **Critical vulnerabilities**: Days to 1 week
- **High vulnerabilities**: 1-2 weeks
- **Medium vulnerabilities**: 2-4 weeks
- **Low vulnerabilities**: Next regular release cycle

**Coordinated Disclosure**: We will work with you to coordinate public disclosure timing. We prefer to:
1. Develop and test a fix
2. Release the patch
3. Publish a security advisory
4. Credit you (if desired) in the advisory

**Credit**: If you would like to be credited for the discovery, please let us know in your report. We will acknowledge your contribution in:
- The GitHub Security Advisory
- The CHANGELOG
- Release notes

### Security Updates

Security fixes are released as:
- **Patch versions** (e.g., v0.1.1 → v0.1.2)
- **GitHub Security Advisories** at https://github.com/SolaceProducts/solace-broker-mcp/security/advisories
- **CHANGELOG** entries with `[SECURITY]` prefix

Subscribe to releases on GitHub to be notified of security updates.

## Security Best Practices

When deploying the Solace Broker MCP Server, follow these security guidelines:

### Production Deployment

- **Use TLS/HTTPS**: Always configure `tls_cert_file` and `tls_key_file` in production
- **Enable authentication**: Set `mcp_client_auth.mode` to `oauth` and configure JWT validation
  - Provide valid `issuer` and `audience` in `mcp_client_auth` config
- **Never use dev tokens in production**: The `dev_token` field (used with `mode: static`) is for local development only

### Credential Management

- **Use environment variables**: Never hardcode credentials in `broker-config.yaml`
  - Use `${VAR_NAME}` syntax for all sensitive values
  - Store credentials in `.env` files (gitignored) or secure secret management systems
- **Restrict file permissions**: Ensure `.env` and config files are readable only by the service user
  ```bash
  chmod 600 .env broker-config.yaml
  ```
- **Rotate credentials regularly**: Follow your organization's credential rotation policy

### Network Security

- **Restrict access**: Use firewall rules or security groups to limit who can access the MCP server
- **Broker connectivity**: Ensure the MCP server can only reach authorized Solace brokers
- **Monitor connections**: Enable logging and monitor for suspicious access patterns

### Kubernetes/Container Security

- **Use secrets**: Store credentials in Kubernetes Secrets, not ConfigMaps
  ```yaml
  apiVersion: v1
  kind: Secret
  metadata:
    name: broker-credentials
  type: Opaque
  stringData:
    username: admin
    password: ${BROKER_PASSWORD}
  ```
- **Run as non-root**: The default container user is non-root for security
- **Read-only filesystem**: Mount config as read-only where possible
- **Resource limits**: Set CPU and memory limits to prevent DoS via resource exhaustion

### Logging Security

- **Credentials are redacted**: The server uses secure logging (see `docs/secure-logging-rules.md`)
- **Review logs before sharing**: Always verify no credentials leaked before sharing logs with support
- **Structured logging**: Use the structured JSON logs for security monitoring and alerting

### Dependency Management

- **Keep dependencies updated**: Run `go get -u ./...` regularly to get security patches
- **Enable Dependabot**: Repository admins should enable Dependabot for automated updates
- **Review security advisories**: Check https://github.com/advisories for Go ecosystem vulnerabilities

### Write and destructive tools

Write tools (`enable_write_tools: true`) let an AI assistant delete queued
messages, disconnect clients, reset statistics, and manage VPN, queue,
topic-endpoint, and REST delivery point configuration. Leave `enable_write_tools`
off unless an operator
deliberately needs these actions, and enable them only with
`mcp_client_auth.mode: oauth` so every invocation is attributable in the audit
log.

## Known Security Considerations

### Broker Credentials

The MCP server stores broker credentials in memory during operation. These are:
- Never logged (enforced by `slog.LogValuer` and `ReplaceAttr`)
- Never exposed via API endpoints
- Protected in the process memory space

However, if an attacker gains shell access to the host running the MCP server, they could potentially dump memory to extract credentials. Mitigations:
- Run the MCP server in a restricted, isolated environment
- Use short-lived broker credentials where possible
- Enable audit logging for broker access

### SEMP API Access

The MCP server makes SEMP (Solace Element Management Protocol) calls to brokers using provided credentials. Ensure:
- Broker credentials have minimum required permissions (principle of least privilege)
- Use read-only SEMP credentials where possible (future: separate read/write tools)
- Monitor broker audit logs for suspicious SEMP activity

### MCP Client Authentication

When `mcp_client_auth.mode` is `"oauth"`:
- MCP clients must provide a valid JWT token
- Tokens are validated against the configured OIDC provider
- Any valid JWT with the correct issuer and audience is accepted

When `mcp_client_auth.mode` is `"disabled"` or `"static"`:
- Static `dev_token` is accepted when `mode: static`
- **Never deploy with `mcp_client_auth.mode: disabled` or `mcp_client_auth.mode: static` in production**

## Vulnerability Disclosure Policy

We follow **coordinated disclosure**:

1. **Private reporting period**: Security researchers report issues privately
2. **Fix development**: We develop and test patches without public disclosure
3. **Coordinated release**: We coordinate disclosure timing with the reporter
4. **Public disclosure**: After the fix is released, we publish a security advisory

We will not pursue legal action against security researchers who:
- Report vulnerabilities responsibly and privately
- Give us reasonable time to fix issues before public disclosure
- Do not exploit vulnerabilities beyond proof-of-concept
- Do not access, modify, or delete data belonging to others

## Security Hall of Fame

We recognize security researchers who help improve the security of this project:

<!--
Format:
- **[Researcher Name](https://github.com/username)** - [Brief description] - [Date]
-->

*No vulnerabilities reported yet. Be the first!*

## Questions?

If you have questions about this security policy, contact [andrea.ross@solace.com](mailto:andrea.ross@solace.com).

For non-security bugs and feature requests, please use [GitHub Issues](https://github.com/SolaceProducts/solace-broker-mcp/issues).
