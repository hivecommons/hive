- Fixed hosted-tenant links in the hub dashboard pointing at the retired
  `hive.kubestellar.io` domain. The dashboard builds `<id>.<domain>` URLs for
  hosted hives that carry no explicit dashboard URL, and it built them from a
  hardcoded hostname, so they kept naming the pre-move domain after the fleet
  moved to `hive.hivecommons.dev`. That is not a cosmetic staleness: the
  retired name is outside the wildcard certificate the fleet now serves, so
  those links did not redirect — the browser refused the TLS handshake and
  showed a certificate warning instead of the tenant's hive. The dashboard now
  takes the parent domain from the server (`hub_spoke_domain`, derived from
  `HIVE_HUB_SPOKE_DOMAIN`) and suppresses the link entirely when the domain is
  not yet known, rather than guessing a host (#5925).

- Fixed the same hardcoded domain in the public landing page's "Contribute Now"
  and "Open Contribute Page" links. That page is static and cannot be handed the
  configured domain, so it now derives it from the host it is served on — which
  is the hub's own host, and therefore the domain hosted spokes hang off — and
  omits the link when it cannot be derived rather than emitting a guessed one
  (#5925).
