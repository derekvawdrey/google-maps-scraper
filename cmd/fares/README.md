# gmaps-fares — origin → destination fare lookup

Adds a directions/fare mode to the google-maps-scraper engine. Give it
`origin -> destination` pairs and it opens Google Maps directions in **transit**
mode for each, waits for the route to render, and extracts the fare, duration,
and a short route summary.

It reuses the scraper's browser engine, concurrency, and CSV/JSON writers. The
extraction lives in [`gmaps/directionsjob.go`](../../gmaps/directionsjob.go);
this command is just the entrypoint.

## Build

The existing Dockerfile builds this binary too (it already bundles Chromium):

```bash
docker build -t gmaps-scraper:fares .
```

## Run

Input is one `<origin> -> <destination>` per line (blank lines and `#` comments
ignored). Origins/destinations can be station names, place names, or full
addresses — anything Google Maps directions accepts.

```bash
# from a file, write CSV
printf 'Tokyo Station -> Shibuya Station\nKyoto Station -> Osaka Station\n' \
  | docker run --rm -i --entrypoint gmaps-fares gmaps-scraper:fares \
      -input stdin -results stdout -lang en

# from a routes file mounted into the container, JSON out
docker run --rm -v "$PWD/routes:/io" --entrypoint gmaps-fares gmaps-scraper:fares \
  -input /io/routes.txt -results /io/fares.json -json -c 2
```

Example output (CSV):

```
origin,destination,travel_mode,fare,duration,route_summary,url,note
Tokyo Station,Shibuya Station,transit,¥210,18 min,18 min | 7:10 AM—7:28 AM | Marunouchi Line  Ginza Line | ¥210 | Details,https://...,
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-input` | `stdin` | input file, or `stdin` |
| `-results` | `stdout` | output file, or `stdout` |
| `-json` | `false` | write JSON instead of CSV |
| `-c` | `1` | concurrent browser pages |
| `-lang` | `en` | Maps language (`hl`), e.g. `en`, `ja` |
| `-mode` | `transit` | `transit`, `driving`, `walking`, `bicycling` |
| `-prefer-bus` | `false` | best-effort: bias transit toward buses |
| `-timeout` | auto | overall safety timeout (auto-scales with route count) |

Set `FARES_DEBUG=1` to dump the raw extracted panel JSON to stderr (useful if
Google changes its DOM and selectors need updating).

## Caveats

- **Fares are not always shown.** Google renders a transit fare only when it has
  the data — good for Japanese rail/metro, spotty for buses and rural operators.
  When absent, the `note` column says so and `fare` is empty.
- **`-prefer-bus` is best-effort.** "Bus-only" is an obfuscated sub-filter, not a
  real travel mode; transit (the default) already includes buses. Use it only if
  you specifically need to bias toward buses.
- **The DOM is obfuscated.** Extraction leans on the stable
  `section-directions-trip-` id prefix plus a currency-regex fallback over the
  panel, so it tolerates class-name churn — but a major Maps redesign could still
  require a selector tweak (see `directionsExtractJS`).
- Scraping Google Maps may be against its Terms of Service; use accordingly.
