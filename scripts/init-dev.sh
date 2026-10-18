#!/bin/sh
set -eu
umask 077
mkdir -p .local
# Preserve keys and database credentials across repeated setup.
if [ ! -e .local/root.key ]; then openssl rand -out .local/root.key 32; fi
if [ ! -e .local/postgres-password ]; then openssl rand -hex 24 > .local/postgres-password; fi
printf '%s\n' 'Development secrets are ready in .local/ (gitignored).'
