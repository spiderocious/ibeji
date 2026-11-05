# Shared keychain cleanup for the end-to-end scripts.
#
# `security delete-generic-password -s <service>` removes ONE item, not every
# account under that service. The daemon stores three — device-seed, api-token
# and pinned-peer — so a single call leaves the pinned peer behind. That did
# not matter while pairing was memory-only: a fresh daemon came up unpaired
# whatever the keychain held. Now that the pin persists, a leftover makes the
# NEXT script start already paired, and it fails to pair with a confusing
# "no link" that looks like a product bug.
#
# Loop until the service is empty.
forget_instance() {
  local svc="com.ibeji.daemon.$1"
  for _ in $(seq 1 10); do
    security delete-generic-password -s "$svc" >/dev/null 2>&1 || break
  done
  rm -rf "$HOME/.ibeji/instances/$1"
}
