# Docker task runner (Linux host prototype)

This is a host-side Go CLI for examining the two-container `preview` / `apply`
workflow. It invokes the Docker CLI using an argument array (never a shell),
and requires Docker CLI access to the local daemon. It uses only Go's standard
library. The included Python image **does not write to the business database**:
`apply` reports `demo_only` results so it is safe to inspect the orchestration.

## Run

```bash
cd docker-job-runner
docker build -t local/job-worker:demo ./example-worker
cp config.example.json config.json
# Work directories default to ./jobs; the runner resolves this to a host path.
go build -o jobrunner .
./jobrunner start  --config config.json --id example001 --type line_models --stage preview --input preview.example.json
./jobrunner status --config config.json --id example001 --stage preview
./jobrunner logs   --config config.json --id example001 --stage preview --tail 100
./jobrunner wait   --config config.json --id example001 --stage preview
cat jobs/example001/preview/output/result.json
./jobrunner start  --config config.json --id example001 --type line_models --stage apply --input approved.example.json
./jobrunner wait   --config config.json --id example001 --stage apply
./jobrunner status --config config.json --id example001 --stage apply
cat jobs/example001/apply/output/result.json
```

`start` returns immediately with the container ID. `wait` blocks until exit and
prints the exit code and Docker's `OOMKilled` field. `status` can be called from
another terminal or after the runner process restarts. `logs` reads the chosen
container's stdout and stderr. `stop` sends a graceful stop request:

```bash
./jobrunner stop --config config.json --id example001 --stage preview
```

Each stage has its own container named `job-<id>-<stage>`. The input file is
copied into the job directory once, then mounted read-only at `/job/input.json`.
The writable output directory is mounted at `/job/output`. The `apply` container
additionally receives the preview output read-only at `/job/preview`. A repeated
`start` for the same job and stage is rejected. Do not remove containers before
capturing exit state and logs. A real manager should copy those into PostgreSQL
and remove old containers according to a retention policy.
The container runs with the UID and GID of the host user invoking `jobrunner`
so it can write its output directory. Run this example as a dedicated non-root
user with Docker access.

The image must provide an `ENTRYPOINT` accepting `--stage`, `--input` and
`--output`, and write `/job/output/result.json`. Existing Python scripts or Go
executables can be wrapped by a small adapter like `worker.py`. Set the fixed
image, memory, CPU, and network in `config.json`; callers cannot choose arbitrary
images or shell commands. Use a specific network instead of `none` when a worker
must reach PostgreSQL or MinIO. For a host-side runner, `work_root` resolves to
a host path and `host_work_root` can be omitted.

## Run this CLI inside the main service container

Use `compose.example.yaml` and `config.in-container.example.json`. The main
image must include both the `jobrunner` executable and the Docker CLI. Mount
the host's Docker socket and the same host job directory into the main service:

- `work_root`: path seen by the main service (`/jobrunner/jobs`), used to write
  input JSON and read results.
- `host_work_root`: path seen by Docker Engine (`/srv/jobrunner/jobs`), used as
  the source of the child containers' bind mounts. Docker Engine never resolves
  a source path inside the main container's private filesystem.

The host directory must exist and be writable by the main service's UID/GID.
The sample child container uses that same numeric UID/GID. A non-root main
service also needs permission to access the socket, commonly via its host GID.
Verify inside the main service with `docker version`, then invoke the CLI as
shown above using `--config /app/jobrunner-config.json`. The worker image must
be present in the host daemon's image store.

## What to add for the production manager

This prototype is a CLI, not an HTTP service or durable scheduler. Persist job
parameters, approval, container ID, per-item status, output version, and logs
in PostgreSQL. Limit concurrency, enforce user authorization and task timeouts,
and validate `result.json` before treating exit code 0 as business success.
Before the real `apply`, recheck source versions and use database transactions
plus a unique generation key for idempotency. Avoid removing a stopped container
until its exit code and diagnostic logs have been saved. Do not mount the Docker
socket inside the public API container.

## Call from a Go main service

The standard-library-only `jobrunnerclient` package wraps the CLI. Build both
executables into the main image. For this demonstration module, import it as
`example.com/docker-job-runner/jobrunnerclient`; in your application, copy the
package into your own module and change the import path.

```go
runner := jobrunnerclient.Client{
    Binary: "/usr/local/bin/jobrunner",
    Config: "/app/jobrunner-config.json",
}
id, err := jobrunnerclient.NewJobID()
if err != nil { return err }
containerID, err := runner.StartPreview(ctx, id, "missing_poles", json.RawMessage(previewParams))
if err != nil { return err }
// Persist id, task type, containerID and state in the main application's DB.
// Return id to the frontend immediately; do not wait in the HTTP handler.

status, err := runner.GetStatus(ctx, id, "preview")
if err != nil { return err }
if status.State.Status == "exited" && status.State.ExitCode == 0 {
    preview, err := runner.ReadResult(ctx, status, 32<<20)
    if err != nil { return err }
    _ = preview // Return candidates to Cesium for inspection.
}

// Only after the user confirms the selected candidate IDs:
applyContainerID, err := runner.StartApply(ctx, id, "missing_poles", json.RawMessage(approvedJSON))
_ = applyContainerID
if err != nil { return err }
```

The main service can expose routes like `POST /api/jobs/preview`,
`GET /api/jobs/:id?stage=preview`, `GET /api/jobs/:id/logs?stage=preview`,
`GET /api/jobs/:id/result?stage=preview`, and `POST /api/jobs/:id/apply`.
Authorize each route against its stored job owner. On restart, poll Docker
through `GetStatus` for jobs previously marked as running. The demo worker's
apply result remains `demo_only` until its adapter calls your actual program.

## Debug a Windows main program against a Linux task host

Build and install `jobrunner` **on the Linux server**, then leave Docker Engine,
the config, images, and the job directory there. The Windows Go program imports
only `jobrunnerclient`. The client runs the remote CLI over SSH, sends input JSON
on standard input, and receives status, logs, and result JSON on standard output.
No Linux file path needs to be mounted on the Windows machine.

```go
runner := jobrunnerclient.Client{
    Remote: "jobrunner@your-linux-host", // configure a trusted SSH account
    SSHPort: 22,                         // set your actual SSH port
    Binary: "/usr/local/bin/jobrunner",   // path on Linux
    Config: "/etc/jobrunner/config.json", // path on Linux
}
id, err := jobrunnerclient.NewJobID()
if err != nil { return err }
containerID, err := runner.StartPreview(ctx, id, "line_models", json.RawMessage(params))
if err != nil { return err }
_ = containerID // Save it with id in the main application's database.

// Poll from another request or background reconciler:
status, err := runner.GetStatus(ctx, id, "preview")
if err != nil { return err }
if status.State.Status == "exited" && status.State.ExitCode == 0 {
    result, err := runner.ReadResult(ctx, status, 32<<20)
    if err != nil { return err }
    _ = result // Send candidates to Cesium.
}
logs, err := runner.Logs(ctx, id, "preview", 100)
_ = logs
if err != nil { return err }
```

On Windows, first connect manually with OpenSSH so the server host key is in
`known_hosts`, and configure SSH key authentication for the account. The client
uses `BatchMode=yes` and strict host key checking; it cannot answer password or
first-contact prompts from an HTTP request. Use a fixed, trusted SSH target and
give that account access only to the needed job files and Docker daemon.

`--input -` is now supported by the CLI for both stages, and `result` prints
validated `result.json` to standard output. Example from a terminal:

```bash
cat preview.example.json | jobrunner start --config config.json \
  --id example002 --type line_models --stage preview --input -
jobrunner result --config config.json --id example002 --stage preview
```

The local mode (`Remote` empty) still works for a main service running on the
Linux host or inside a container that has the runner binary and Docker access.

## Run the Linux jobrunner inside its own SSH container

This optional deployment keeps the Windows and production Go clients on the
same `jobrunnerclient` API. They both connect by SSH to the **jobrunner
container**, while that container controls the Linux host's Docker Engine.
Use `Dockerfile.ssh`, `ssh-entrypoint.sh`, `sshd_config`, and
`compose.ssh.example.yaml`.

On the Linux host, build the Linux runner binary and prepare persistent paths:

```bash
cd docker-job-runner
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o jobrunner .
install -d -m 0700 /srv/jobrunner/ssh/hostkeys
install -d -m 0700 /srv/jobrunner/ssh
install -d -o 10001 -g 10001 -m 0700 /srv/jobrunner/jobs
cp config.in-container.example.json /srv/jobrunner/config.json
# Put only trusted clients' SSH public keys into this file.
touch /srv/jobrunner/ssh/authorized_keys
chmod 0644 /srv/jobrunner/ssh/authorized_keys
```

The `authorized_keys` file must contain one public key per trusted Windows or
production client. Do not copy a private key into the image. Configure a
reachable IP and deploy:

```bash
JOBRUNNER_BIND_IP=YOUR_SERVER_IP docker compose -f compose.ssh.example.yaml up -d --build
docker compose -f compose.ssh.example.yaml logs -f jobrunner
```

The persistent host key directory keeps SSH identity stable across container
recreation. Firewall port 2222 to trusted clients. Test from Windows after
verifying the host key fingerprint through a separate trusted channel:

```powershell
ssh -p 2222 jobrunner@YOUR_SERVER_IP 'id; docker version'
```

For Windows debugging set `Remote="jobrunner@YOUR_SERVER_IP"`, `SSHPort=2222`,
`Binary="/usr/local/bin/jobrunner"`, and
`Config="/etc/jobrunner/config.json"`. In production, use the same Go code
with environment-driven SSH destination and port. If the production main
service shares this Compose network, it can use the `jobrunner` service name
and port 22; it needs an SSH client, its own private key, and a trusted
`known_hosts` entry. Do not mount the Docker socket in that main service.

`work_root` is `/jobrunner/jobs` inside the runner container, while
`host_work_root` is `/srv/jobrunner/jobs` for the Docker daemon's bind mounts.
The SSH login user has UID 10001; the entrypoint adds it to the socket's
numeric group before starting sshd. Access to the Docker socket grants broad
control over the host, so this container and its SSH keys should be treated as
privileged infrastructure.
