# Installing Vedetta

Vedetta comes in two packages. Both need Docker with Compose, or Podman (see
[Podman](#podman): two lines to run once, and on every command below `podman compose`
instead of `docker compose`), on amd64 or arm64. They also need `git`; on a minimal
system install it first (`sudo apt install git`, `sudo dnf install git`).

| | What runs | For whom |
|---|---|---|
| **essential** | Vedetta + SearXNG (search) + Jina Reader (page reader) | starting from zero |
| **core** | only Vedetta (about 30 MB) | you already have SearXNG, or a key for a paid search |

In both cases **you bring the model**: any OpenAI-compatible service that can use
tools. It can be in the cloud, with your key, or on your machine (Ollama, LM Studio,
llama.cpp, vLLM…). The first run in the browser asks for it.

## essential

```
git clone https://github.com/peppeg/vedetta
cd vedetta/deploy/essential
cp .env.example .env
echo "SEARXNG_SECRET=$(openssl rand -hex 32)" >> .env
docker compose up -d
```

If it stops with a long error, don't panic. Near the end, a line says `required variable
… is missing a value`: it names the value to write in `.env`.

The first start downloads about 1.8 GB, mostly Jina Reader. The whole package takes
about 4 GB of disk and up to 1.5 GB of memory.

Then open **http://localhost:3030**. The first run finds SearXNG and Jina by itself and
asks for the model.

## core

```
git clone https://github.com/peppeg/vedetta
cd vedetta/deploy/core
cp .env.example .env
docker compose up -d
```

Then open **http://localhost:3030**. The first run looks for SearXNG, degoog and Jina
Reader at the usual addresses. If it does not find them, write their addresses, or choose a
paid search and paste its key.

- **Inside a container, "localhost" is the container itself.** A service on the same
  machine is at `host.docker.internal` (Docker) or `host.containers.internal`
  (Podman, where both names work), for example `http://host.docker.internal:8888`.
- **A service on this machine must listen on all its addresses**, not only on
  127.0.0.1, or the container cannot reach it. Ollama listens only on 127.0.0.1 at
  first: start it with `OLLAMA_HOST=0.0.0.0`. Then whoever is on your network can
  reach it too, unless a firewall stops them. The same holds for a model used with
  essential.
- **Your SearXNG must answer in JSON**, and its limiter must be off, or let Vedetta
  through. If one of these is wrong, the first run says so. The `settings.yml` SearXNG
  writes for itself has no `search` part: add it, then restart SearXNG. If the file
  belongs to another user (rootless Podman), edit it with
  `podman unshare nano settings.yml`.

  ```yaml
  search:
    formats:
      - html
      - json
  ```

The two packages use the same name and the same data volume: to change from one to
the other, stop the first (`docker compose down`, without `-v`), then start the second.
Searches and settings stay. core does not include SearXNG and Jina Reader: the addresses
essential used (`http://searxng:8080`, `http://jina:8081`) will not answer. In
**Settings → Services**, write the addresses of your own services, or choose a paid
search.

## Reaching Vedetta from other devices

At first Vedetta answers only on the machine where it runs. To reach it from the
phone or another computer at home:

1. In `.env`: `VEDETTA_PUBLISH=0.0.0.0`
2. `docker compose up -d`
3. Open Vedetta, then **Settings → Access**, and set an access code. Each device asks
   for it once and remembers it for a year.

The access code also opens the settings. To keep those for yourself, set a settings code
too: in **Settings → Access** before step 1, while only this machine reaches Vedetta, or
in `.env` (`VEDETTA_CONFIG_TOKEN`). From another device the first one cannot be set, so
that no guest can close you out.

Behind a reverse proxy with your own domain, also add the domain to `.env`, for
example `VEDETTA_ALLOWED_HOSTS=vedetta.example.org`.

## What has been tried

- **Docker** 28 with Compose 2.38 on Ubuntu 24.04, amd64: both packages, down to an
  answer, at every change of what is installed (`deploy/test/install.sh`, run by GitHub
  with a stand-in model).
- **Podman** 6.1.3 without root, `podman-compose` 1.6.0, on Fedora 45: both packages by
  hand, with a real model, reboot and logout from ssh included.
- Not tried: Docker Desktop (Windows, macOS); on arm64 the image is built, not started.

## Updating

```
git pull
docker compose up -d
```

The compose files name the exact versions of every image: a new Vedetta release
changes them.

## Removing everything

```
docker compose down -v
```

`-v` also removes the data volume: searches, memory, settings. Vedetta's log goes
away with the container. To remove the downloaded images too (3.7 GB):
`docker compose down -v --rmi all`.

Then delete the `vedetta` folder: its `.env` holds the options you set there and, with
essential, the SearXNG secret. With Podman, if you ran the two lines in
[Podman](#podman) only for Vedetta, undo them:
`systemctl --user disable podman-restart.service` and
`sudo loginctl disable-linger $USER`.

## Podman

`podman compose up -d` works the same, as a normal user (no root). It needs a compose
provider: we tried `podman-compose` 1.6.0 with Podman 6.1.3 on Fedora 45
(`sudo dnf install podman-compose`). The first line it prints, `>>>> Executing external
compose provider`, is normal.

**Podman stops Vedetta when you log out**, about 10 seconds after your last session
ends: on a machine you reach with ssh, as soon as you close it. After a reboot it does
not start Vedetta again. Once, right after the first `podman compose up -d`:

```
systemctl --user enable podman-restart.service
sudo loginctl enable-linger $USER
```

The first line starts Vedetta again after a reboot; the second keeps it running when
you are not logged in.

**Do not use `podman compose restart`:** with podman-compose it often leaves Vedetta
as it was, without saying so. Use `podman compose down` and `podman compose up -d`, or
restart one service: `podman compose restart vedetta`.

## If something does not work

- The first run, and **Settings → Services**, say what is wrong with SearXNG or Jina:
  not reachable, not SearXNG, JSON off, limiter, no results.
- From the folder of the package (`deploy/essential` or `deploy/core`):
  - `docker compose ps`: every service should be `healthy`.
  - `docker compose logs vedetta`: the first lines say where Vedetta looks for each
    service and who can reach it.

## Good to know

- **Jina Reader follows redirects by itself**, outside Vedetta's address filter. It can
  reach whatever this machine reaches, home network included.
- **Everything stays on your machine**, in the `vedetta-data` volume. Nothing is sent to
  the project.
