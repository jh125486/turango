# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in turango, please report it using GitHub's private vulnerability reporting feature:

1. Navigate to the [Security tab](https://github.com/jh125486/turango/security) of this repository
2. Click "Report a vulnerability" or "Advisories" → "New draft security advisory"
3. Provide details about the vulnerability (description, affected versions, remediation steps if known)

This mechanism ensures your report is reviewed privately by the repository maintainers before any public disclosure.

## Scope

### In Scope
Security vulnerabilities in turango itself — its CLI, core mutation-testing engine, and dependencies.

### Out of Scope
The `corpus/*/module` directories contain frozen, historical copies of Go standard library source code preserved as regression test fixtures. Vulnerabilities discovered in these copies should be reported upstream to the Go project, not here.

## Response Timeline

This is a solo-maintained, pre-1.0 project. Security issues will be reviewed and addressed on a best-effort basis without a guaranteed SLA. Response times may vary depending on issue severity and maintainer availability.

## Disclaimer

turango is provided as-is. While security is taken seriously, this project makes no guarantees about the completeness of security reviews or the absence of vulnerabilities.
