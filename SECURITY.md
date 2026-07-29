# Security Policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Use GitHub's private reporting instead: go to the **Security** tab of this
repository and choose **Report a vulnerability**. That opens a private advisory
visible only to the maintainers.

Include what you did, what happened, and what you expected. A minimal reproduction
helps more than anything else. Expect an initial reply within a few days.

## What is in scope

kirogo handles live AWS credentials on your machine, so the interesting classes of
problem are:

- Credential leakage: a token appearing in a log line, an error message, an HTTP
  response, or a crash dump
- Authentication bypass on any endpoint that is supposed to require
  `PROXY_API_KEY`
- Request smuggling or response splitting through the translation layer
- A crafted upstream response that causes memory exhaustion or a panic in the
  event stream decoder
- Path traversal or unintended writes through credential file handling

## What is not in scope

- Running kirogo on `0.0.0.0` with the default `PROXY_API_KEY`. That is documented
  as unsafe, and kirogo warns about both at startup.
- Quota exhaustion by someone you gave your API key to.
- Behaviour of the upstream Kiro or AWS services themselves. Report those to
  Amazon.
- Vulnerabilities in Kiro IDE or kiro-cli. kirogo only reads their credential
  files.

## Handling credentials

If you are filing any issue, security or otherwise:

- Never paste a real `accessToken`, `refreshToken` or `PROXY_API_KEY`
- Redact the account id inside a `profileArn`
- `LOG_LEVEL=DEBUG` output is designed to be safe to share — tokens are redacted
  at every level — but read it over before posting

If you believe a token of yours has been exposed, sign out of Kiro and back in to
rotate it.
