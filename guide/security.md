# Security

Vedetta is meant for a home network, not for the open internet.

## Who can reach it

- **At first, only the machine it runs on.** Opening it to other devices is a choice (see
  the [install guide](../deploy/README.md#reaching-vedetta-from-other-devices)). Without an
  access code, whoever is on the network can then use it; Vedetta says so at start and in
  the settings.
- **Access code.** Each device types it once and is remembered for a year. Changing it
  signs every device out. Wrong tries are limited.
- **Settings code.** A second code, for services, models and data only. From another
  device the first one cannot be set.
- Codes set from the interface are kept only as a salted fingerprint.
- **Names.** Vedetta answers to IP addresses and home names. Any other name, such as your
  own domain behind a reverse proxy, must be listed (`VEDETTA_ALLOWED_HOSTS`): this stops
  a site from pointing its name at your home.
- **Other sites** cannot call Vedetta from your browser, unless you list them
  (`VEDETTA_CORS_ORIGINS`).

There is no HTTPS inside Vedetta: from outside the home, use a VPN or a reverse proxy
with HTTPS.

## What it reads

Pages on the web are not trusted.

- **Home network.** The built-in reader and the browser reader refuse addresses of the
  local network, also after a redirect.
- **Jina Reader is the exception:** it follows redirects by itself, outside this filter. It
  can reach whatever its machine reaches. Vedetta refuses to give it the searches, but
  other services of your home may answer it: use an access code on them.
- **Instructions hidden in a page.** The text of a page is given to the model marked as
  content to read, not as orders. No mark stops a model with certainty: what limits the
  damage is that the model has two tools only, searching and reading.

## Keys

A key written as `${NAME}` stays in `.env`. Keys saved from the interface are in
`data/config.local`, readable only by the user Vedetta runs as. A key is never sent to
a host different from the one it was set for, nor shown again in the interface.

## The container

The image has no shell and runs as a normal user. The packages give it a read-only file
system, except for its data, and no privilege.
