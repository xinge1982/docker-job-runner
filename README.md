# Docker task runner (Linux host prototype)

This is a Go CLI for direct tasks and the two-container `preview` / `apply`
workflow. It invokes the Docker CLI using an argument array (never a shell),
and requires Docker CLI access to the local daemon. It uses only Go's standard
library. The included Python image **does not write to the business database**:
`apply` reports `demo_only` results so it is safe to inspect the orchestration.

## Run

```bash
cd docker-job-runner
docker build -t local/job-worker:demo ./example-worker
cp config.demo.example.json config.json
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

For staged tasks without a configured `command`, the image must provide an
`ENTRYPOINT` accepting `--stage`, `--input` and `--output`, and write
`/job/output/result.json`. Existing scripts can use a small adapter like
`worker.py`. The `config.demo.example.json` file preserves this workflow.
The fixed image, memory, CPU, network, mounts, and command come from the task
configuration. For a host-side runner, `work_root` resolves to a host path and
`host_work_root` can be omitted.

## Run existing binaries or Python scripts in standard images

`config.example.json` runs on the host. `config.in-container.example.json`
runs in the SSH jobrunner container. Both configure `tileset_build` and
`feature_road_id` with `alpine:3.14`, and a replaceable `missing_poles`
example with `python:3.11-alpine`. These tasks use `mode: "direct"` and
`--stage run`: they execute once without the preview/apply gate. The Python
script path and flags are placeholders; adapt them to your existing program.
For a real preview/apply task, use `mode: "staged"` and implement the result
contract before enabling the apply step.

Place executable binaries under `/srv/jobrunner/programs` on the Docker host
and make them executable there. Alpine requires binaries compatible with musl
or statically linked binaries. Put network data under
`/srv/jobrunner/networks`. Edit the host-side paths in `mounts[*].source` for
your installation. Each `target` and `work_dir` is a path *inside the task
container*. `/job/input.json` holds the submitted JSON and `/job/output` is
the stage's writable output directory. Configure a job result writer to create
`/job/output/result.json` if you want to use the `result` command; `status`,
`wait`, and `logs` work without that file. The two Go examples write their
business files in the configured `networks` mount.

Create a private environment file visible to the runner. Use
`/srv/jobrunner/worker.env` for the host CLI, or mount that file into the SSH
runner at `/etc/jobrunner/worker.env` as shown in
`compose.ssh.example.yaml`. Add your actual database and storage variables
there, one `NAME=value` entry per line; do not commit this file. Keep
`config.json` private when it contains deployment-specific paths. The Docker
CLI reads `env_file` from the runner filesystem. Docker Engine resolves
`mounts[*].source` on the *host*, including when the CLI runs inside the SSH
runner container. The jobrunner container does not need the programs or network
data mounted into itself.

Submit the program's command flags with `--params` as a JSON object. Each
accepted name, target flag, required value, and optional regular expression
is declared under `parameters` in the task configuration. The runner appends
each `-flag=value` as a separate process argument, without a shell. Unknown
names, missing required parameters, and values that fail their pattern are
rejected. The separate `--input` JSON file remains available to the program
at `/job/input.json`.

```bash
cp config.example.json config.json
printf 'POSTGRES_HOST=postgres\nPOSTGRES_PORT=5432\nPOSTGRES_USER=postgres\n' > /srv/jobrunner/worker.env
chmod 0600 /srv/jobrunner/worker.env
printf '{}\n' > run.example.json
./jobrunner start --config config.json --id bridges001 --type tileset_build --stage run \
  --input run.example.json \
  --params '{"config":"networks/config_jiangsu_1031.yaml","output_path":"networks/network_jiangsu_1031/tilesets/bridges","tileset_type":"bridges"}'
./jobrunner status --config config.json --id bridges001 --stage run
./jobrunner logs --config config.json --id bridges001 --stage run
./jobrunner wait --config config.json --id bridges001 --stage run
```

Add the remaining required database and storage settings to the environment
file locally. Make sure the Docker network `sign_default` exists. The runner
does not create it. Run jobrunner under an account that can read the environment
file and use Docker. A direct task can also be submitted through the Go client:

```go
containerID, err := runner.StartRunWithParams(ctx, id, "feature_road_id",
    json.RawMessage(`{}`), map[string]string{
        "config": "networks/config_jiangsu_2026.yaml",
        "road_area_table": "hdroad_area0924",
        "feature_table": "hdtraffic_ene",
    })
_ = containerID
if err != nil { return err }
status, err := runner.GetStatus(ctx, id, "run")
```

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
install -d -m 0755 /srv/jobrunner/programs /srv/jobrunner/networks
cp config.in-container.example.json /srv/jobrunner/config.json
# Add the required variables locally and let the jobrunner user read this file.
touch /srv/jobrunner/worker.env
chown 10001:10001 /srv/jobrunner/worker.env
chmod 0600 /srv/jobrunner/worker.env
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
`mounts[*].source` in the task configuration also refers to paths on the
Docker host. `env_file` instead refers to `/etc/jobrunner/worker.env` inside
the runner container; Compose mounts the host environment file there.
The SSH login user has UID 10001; the entrypoint adds it to the socket's
numeric group before starting sshd. Access to the Docker socket grants broad
control over the host, so this container and its SSH keys should be treated as
privileged infrastructure.

## Add a Windows SSH key and connect to the jobrunner container

The `compose.ssh.example.yaml` file mounts the host's
`/srv/jobrunner/ssh/authorized_keys` at
`/home/jobrunner/.ssh/authorized_keys` inside the container. Container port 22
is published as port 2222 on the host. Add your public key through your
existing **SSH connection to the Linux host** before connecting to the
jobrunner container.

### 1. Generate a dedicated key in Windows PowerShell

```powershell
ssh-keygen -t ed25519 -f "$env:USERPROFILE\.ssh\jobrunner_ed25519" -C "jobrunner-windows"
```

`jobrunner_ed25519` is the private key; keep it on Windows. Upload only
`jobrunner_ed25519.pub`. If the key file already exists, decide whether to
reuse it before running this command. Do not overwrite an existing private key.

### 2. Append the public key to authorized_keys on the Linux host

Replace `HOST_SSH_PORT`, `HOST_ADDRESS`, and `root` with the port, address, and
account you already use to access the **Linux host**. That account must be
able to write to `/srv/jobrunner/ssh`.

```powershell
$pub = "$env:USERPROFILE\.ssh\jobrunner_ed25519.pub"

Get-Content $pub |
  ssh -p HOST_SSH_PORT root@HOST_ADDRESS `
    "install -d -m 0700 /srv/jobrunner/ssh; touch /srv/jobrunner/ssh/authorized_keys; cat >> /srv/jobrunner/ssh/authorized_keys; chmod 0644 /srv/jobrunner/ssh/authorized_keys"
```

Check the file on the host: the new public key should occupy one line starting
with `ssh-ed25519`. Never copy the private key into `authorized_keys` or the
image. Running the append command twice creates a duplicate line.

If the container previously exited because `authorized_keys` was empty, start
or recreate it from the repository directory:

```bash
JOBRUNNER_BIND_IP=YOUR_SERVER_IP docker compose -f compose.ssh.example.yaml up -d --build
```

Allow access to the published port 2222 only from trusted development and
production clients in the server firewall.

### 3. Verify the container's SSH host key and connect

After the container starts, obtain its persistent host key fingerprint through
your existing SSH connection to the Linux host:

```bash
ssh-keygen -lf /srv/jobrunner/ssh/hostkeys/ssh_host_ed25519_key.pub
```

On the first Windows connection, compare the ED25519 fingerprint in the SSH
prompt with that value. Accept it only if they match; SSH then records it in
the Windows `known_hosts` file.

```powershell
ssh -i "$env:USERPROFILE\.ssh\jobrunner_ed25519" `
    -p 2222 jobrunner@HOST_ADDRESS `
    "id; docker version"
```

`id` should show the `jobrunner` user and the group that can access the Docker
socket. `docker version` should show the Docker Server on the Linux host.
You can also query a task:

```powershell
ssh -i "$env:USERPROFILE\.ssh\jobrunner_ed25519" `
    -p 2222 jobrunner@HOST_ADDRESS `
    "/usr/local/bin/jobrunner status --config /etc/jobrunner/config.json --id example001 --stage preview"
```

If `example001` does not exist yet, a container-not-found response is expected.

### 4. Debug the Go main program from Windows

`jobrunnerclient` invokes the system `ssh` command and does not have its own
private-key-path field. Add this entry to `$env:USERPROFILE\.ssh\config`:

```sshconfig
Host jobrunner-dev
    HostName HOST_ADDRESS
    User jobrunner
    IdentityFile ~/.ssh/jobrunner_ed25519
    IdentitiesOnly yes
```

Verify the alias in PowerShell:

```powershell
ssh -p 2222 jobrunner-dev "docker ps"
```

Use that alias in the Go main program:

```go
runner := jobrunnerclient.Client{
    Remote:  "jobrunner-dev",
    SSHPort: 2222,
    Binary:  "/usr/local/bin/jobrunner",
    Config:  "/etc/jobrunner/config.json",
}
```

The client enables `BatchMode=yes` and strict host key checking. It cannot
prompt for an SSH password or first-connection confirmation during a request.
If the private key has a passphrase, add it to your SSH agent with `ssh-add`
before debugging. Give the production main program its own key and
`known_hosts` entry. Configure the destination and port per environment;
the Go job workflow stays the same.
