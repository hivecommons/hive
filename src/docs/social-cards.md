# Contributor social cards

Hive serves public, cacheable social-card SVGs for data that is already visible on public contributor surfaces. The cards are 1200×630 Open Graph size and never require dashboard login.

## URLs

- Player card: `/cards/player/<github-login>.svg`
- Achievement card: `/cards/achievement/<github-login>/<achievement-id>.svg`
- Leaderboard card: `/cards/leaderboard/contributors.svg`, `/cards/leaderboard/teams.svg`, or `/cards/leaderboard/swarm.svg`
- Share landing pages: `/share/player/<github-login>`, `/share/achievement/<github-login>/<achievement-id>`, and `/share/leaderboard/<board>`

The OpenAPI-documented aliases under `/api/cards/...` return the same SVG bytes
for API clients; the shorter `/cards/...` URLs are the canonical URLs to share.

Share landing pages include `og:image` and `twitter:card` metadata pointing at the matching card and put the real public dossier or leaderboard link first. The only onboarding call-to-action is a small link at the bottom.

## Embeds

Use the dashboard share buttons next to leaderboard rows and dossier achievements to copy a URL, Markdown link, or HTML link. Example:

```markdown
[Hive contributor alice](https://example.com/share/player/alice)
```

## Format note

The MVP ships SVG cards without adding a rasterizer dependency. Most modern browsers render the cards directly. Some social platforms do not render SVG `og:image`; for those platforms, share the landing page link and document that raster PNG generation is deferred until Hive already carries a maintained rasterizer dependency.
