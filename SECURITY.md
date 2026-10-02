# Security policy

## Reporting a vulnerability

Please **don't** open a public issue for a security problem. Use GitHub's private
vulnerability reporting instead: the repository's **Security** tab → **Report a
vulnerability**.

Include what you found, how to reproduce it, and the commit or release you tested. You'll
get an acknowledgement within a week.

## Supported versions

Security fixes go to `staging` and are released to `main`. Run the latest `main`.

## Scope notes

- The admin website and the staff gateway are designed to be reachable only through an SSH
  tunnel. A deployment that exposes them to the internet is outside the supported setup.
- Never commit `deploy/.env` or `deploy/secrets/`; they hold your deployment's passwords
  and signing keys.
