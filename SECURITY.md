# Security Policy

## Supported versions

Cadre has no release yet. Once it does, the latest minor release receives security fixes.

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it through GitHub's private vulnerability reporting: open the **Security** tab of this repository and choose **Report a vulnerability**. Include the affected component, a description, and steps to reproduce.

You will get an acknowledgement within a few days. Once a fix is ready we publish an advisory and credit the reporter unless they prefer otherwise.

## Scope

Cadre handles team credentials (model provider keys, tool server tokens) and executes tools on behalf of agents. Reports about credential exposure, fencing or lease bypasses that let a task run twice, privilege escalation across teams, or tool permission bypasses are especially welcome.
