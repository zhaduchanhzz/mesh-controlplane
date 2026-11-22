#!/bin/sh
# Installed as /etc/letsencrypt/renewal-hooks/deploy/mc-cp-headscale.sh.
# Copies the renewed cert where the unprivileged headscale user can read it.
set -eu
src=/etc/letsencrypt/live/cp.gta5web.online
dst=/etc/headscale/tls
install -d -m 0750 -o root -g headscale "$dst"
install -m 0644 -o root -g headscale "$src/fullchain.pem" "$dst/fullchain.pem"
install -m 0640 -o root -g headscale "$src/privkey.pem" "$dst/privkey.pem"
systemctl try-restart headscale.service
