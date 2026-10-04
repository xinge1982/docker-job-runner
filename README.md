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
go build -o jobrunner ./cmd/jobrunner
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

Use `--response-format json` when a caller needs structured startup metadata.
The response contains the job ID, stage, container ID, deterministic container
name, start time, and—when runner debug is enabled—the redacted Docker `create`
and `start` commands. The same metadata is saved as
`<work_root>/<job-id>/<stage>/start.json` and is included in job archives.
The Go client exposes `StartRunRequestDetailed`,
`StartPreviewRequestDetailed`, and `StartApplyRequestDetailed` for this form.

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

### Large input files and job archives

Keep large GeoJSON files out of the 32 MiB start-request JSON. Upload each
file before starting the stage. The upload is streamed through SSH stdin to
`<work_root>/<job-id>/files/`; it is published only after the exact size and
optional SHA-256 digest match. Names are simple file names, not paths. A
published file cannot be overwritten. The task container sees the shared
files directory read-only at `/job/files/` for preview, apply, and direct runs.

```bash
size=$(wc -c < roads.geojson)
digest=$(sha256sum roads.geojson | cut -d ' ' -f 1)
jobrunner upload --config config.json --id roads001 \
  --file-name roads.geojson --size "$size" --sha256 "$digest" < roads.geojson
jobrunner archive --config config.json --id roads001 > roads001.tar.gz
```

For a remote SSH runner, pipe the file to the same commands over SSH. The
`archive` command streams a gzip-compressed tar of the entire instance
directory to stdout. It tries to save Docker logs to each stage's `logs/`
directory before packaging. A running stage can still change files while
archiving, so download after completion for a consistent copy. The archive
rejects symlinks and special files. Uploads are limited to 8 GiB per file;
archives are limited to 16 GiB of uncompressed regular files.

The Go client offers the same operations for local and SSH deployments:

```go
file, err := os.Open("roads.geojson")
if err != nil { return err }
defer file.Close()
info, err := file.Stat()
if err != nil { return err }
uploaded, err := runner.UploadFile(ctx, id, "roads.geojson", info.Size(), "", file)
if err != nil { return err }
// Pass uploaded.Path as a validated job parameter or in the small input JSON.
_ = uploaded.Path // /job/files/roads.geojson

archive, err := os.Create("roads001.tar.gz")
if err != nil { return err }
defer archive.Close()
if err := runner.DownloadArchive(ctx, id, archive); err != nil { return err }
```

Supply the expected SHA-256 hex string in the fifth `UploadFile` argument
when it is available. Both methods stream through `io.Reader` / `io.Writer`
and use the caller's context; set a suitable deadline for large transfers.
If an archive download fails, discard the partial output file. Authorize
the job ID against its owner in the main application's HTTP handlers before
calling these methods.

### Retain and clean up completed jobs

Jobrunner does not automatically remove stopped containers or job directories.
List instances with at least one retained stage container whose existing
stages have all exited, then explicitly delete one instance when your main
application has finished retaining its data:

```bash
jobrunner list-completed --config config.json
jobrunner archive --config config.json --id roads001 > roads001.tar.gz
jobrunner delete --config config.json --id roads001
```

`list-completed` returns a JSON array of `job_id` and `stages` with Docker
states and exit codes. A failed stage with a nonzero exit code is still a
completed instance. A finished `preview` can appear before the user starts
`apply`; the main application's database determines when the entire business
workflow is ready for cleanup. An upload-only directory has no container, so
it is not included in this list, but can still be deleted by ID.

`delete` checks all existing preview, apply and run containers for the ID,
refuses running stages, verifies their jobrunner labels, removes the stopped
containers, then removes the instance directory. A failed removal preserves
the directory and can be retried. Archive before deleting if logs and files
need to be retained. Jobrunner does not control Docker's external pruning;
exclude retained job containers from any host-level cleanup policy.

```go
completed, err := runner.ListCompleted(ctx)
if err != nil { return err }
_ = completed // Reconcile with job ownership and workflow state in your DB.
// After preserving the archive, and only when the application requests cleanup:
if err := runner.DeleteJob(ctx, id); err != nil { return err }
```


### Environment-based runner configuration

`jobconfig.Load` reads JSON through Viper. Set `JOBRUNNER_`-prefixed
environment variables to override fixed scalar settings. Use two underscores
between nested names; task names in variable names are uppercase. For example:

```bash
export JOBRUNNER_WORK_ROOT=/srv/jobrunner/jobs
export JOBRUNNER_HOST_WORK_ROOT=/srv/jobrunner/jobs
export JOBRUNNER_DEBUG=true
export JOBRUNNER_TASKS__TILESET_BUILD__NETWORK=sign_default
```

Supported task fields are `image`, `memory`, `cpus`, `network`, `mode`,
`schema_argument`, `work_dir`, and `env_file`. For mount source paths in JSON,
use `${PROGRAM_PATH}` placeholders, such as
`"source": "${PROGRAM_PATH}/tileset-lod-tool"`, and set `PROGRAM_PATH` in
the runner process environment. An unset placeholder stops config loading.
The command, mount targets, and declared job parameters remain fixed in JSON.
Job connection details and credentials belong in each start request's
`environment` values; `JOBRUNNER_` variables configure the runner itself.

Set top-level `"debug": true` or `JOBRUNNER_DEBUG=true` to print the Docker
`create` and `start` command lines to standard error before a job container is
started. Values passed with `--env` / `-e` are replaced with `<redacted>`, and
`--env-file` paths are also hidden. Variable names remain visible so the
effective command can be diagnosed. The redaction is limited to Docker
environment options: do not put credentials in task command arguments,
container names, image names, labels, mount paths, or other Docker options.


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

The task definition fixes the image, command, work directory, mounts, network
and resource limits. Set `schema_argument: "--jobrunner-schema"` once the
configured program supports the schema protocol below. The runner then reads
`parameters` and `environment_variables` from the program at each
`describe` and `start` call; they do not have to be duplicated in
`config.json`. A task without `schema_argument` keeps the previous static
declarations for compatibility. When discovery is enabled, its output is
authoritative.

An uploaded file parameter can declare `"type":"path"` and
`"path_prefix":"/job/files/"`. The runner validates the submitted path
before passing it to the configured command. Other absolute path prefixes
remain unsupported.

Each parameter declares `name`, target `flag`, and `type` (`string`,
`path`, `integer`, `number`, or `boolean`), plus `required` and optional
`pattern` or `allowed_values`. Paths require `path_prefix`; the runner
rejects traversal. Validated values become separate `-flag=value` process
arguments, without a shell. Environment declarations contain names and
`required` / `secret` metadata, never addresses or credentials. The fixed
`environment` map remains suitable for public defaults such as `TZ`.

Docker Engine resolves `mounts[*].source` on the *host*, including when the
runner CLI runs inside the SSH container. The runner container does not need
the program or network directories mounted into itself. The optional legacy
`env_file` setting remains available for deployments that supply a private
file on the runner filesystem; new tasks use `environment_variables`.

Use `jobrunner describe --config config.json --type tileset_build` to retrieve
the discovered task object. From the main program, call
`runner.DescribeTask(ctx, "tileset_build")`, inspect `Parameters` and
`EnvironmentVariables`, then submit `jobconfig.StartRequest`. The request
JSON travels over stdin, including when the client connects over SSH.
If the main program has a local copy of the task catalog, it can instead use
`jobconfig.Load(path)` and `config.Task(name)` for fixed fields only.
`DescribeTask` reads the active remote program's parameter declarations;
local config alone cannot discover a program's schema.
Environment values are passed to the Docker CLI through its process
environment with `--env NAME` and never appear in the CLI or SSH argument
list. The Docker daemon still stores container environment values in its
container metadata; restrict access to the daemon accordingly.

### Schema protocol implemented by each job program

With `schema_argument` enabled, jobrunner runs the configured image and
`command` followed by exactly `--jobrunner-schema`. The program must print
one JSON object on stdout and exit 0 without doing business work. It must not
need database, object storage, or network access for this operation. Human
`--help` output remains free-form; use the dedicated argument for automation.
For a binary with subcommands, the invocation is
`program subcommand --jobrunner-schema`.

```json
{
  "version": 1,
  "parameters": [
    {
      "name": "config",
      "flag": "-c",
      "type": "path",
      "required": true,
      "path_prefix": "networks/",
      "pattern": "networks/[A-Za-z0-9_./-]+[.]yaml"
    },
    {
      "name": "tileset_type",
      "flag": "-tileset-type",
      "type": "string",
      "required": true,
      "allowed_values": ["bridges", "roads", "signs"]
    }
  ],
  "environment_variables": [
    {"name": "POSTGRES_HOST", "required": true},
    {"name": "POSTGRES_PASSWORD", "required": true, "secret": true}
  ]
}
```

The response is limited to 64 KiB and only version 1 is accepted. Discovery
runs with no network, no worker credentials or environment file, a read-only
filesystem, read-only copies of the configured mounts, limited resources, and
a short timeout. Keep the image present on the Docker host; discovery does
not pull images. Implement the flag in the existing Go or Python job before
using the updated `config.example.json` or
`config.in-container.example.json`. These files now enable discovery for
all three standard-image jobs. The bundled demo worker also implements the
flag, so `config.demo.example.json` can be run immediately.

```bash
cp config.example.json config.json
./jobrunner describe --config config.json --type tileset_build
# Create a private request.json with input, parameters and environment.
./jobrunner start --config config.json --id bridges001 --type tileset_build \
  --stage run --request request.json
./jobrunner status --config config.json --id bridges001 --stage run
./jobrunner logs --config config.json --id bridges001 --stage run
./jobrunner wait --config config.json --id bridges001 --stage run
```

For manual testing, `request.json` has this shape. Replace the placeholder
values locally; never commit a request containing credentials:

```json
{
  "input": {},
  "parameters": {
    "config": "networks/config_jiangsu_1031.yaml",
    "output_path": "networks/network_jiangsu_1031/tilesets/bridges",
    "tileset_type": "bridges"
  },
  "environment": {
    "POSTGRES_HOST": "postgres",
    "POSTGRES_PORT": "5432",
    "POSTGRES_USER": "postgres",
    "POSTGRES_PASSWORD": "<set-locally>",
    "MINIO_OUTPUT_HOST": "minio",
    "MINIO_OUTPUT_PORT": "59000",
    "MINIO_OUTPUT_ACCKEY": "<set-locally>",
    "MINIO_OUTPUT_SECKEY": "<set-locally>"
  }
}
```

The request file is useful for manual testing; protect it because it contains
credentials. An application should use `--request -` over stdin instead. Make
sure the Docker network `sign_default` exists. A direct task can be submitted
through the Go client using the same contract:

```go
task, err := runner.DescribeTask(ctx, "feature_road_id")
if err != nil { return err }
// task.Parameters and task.EnvironmentVariables describe the required fields.
_ = task
containerID, err := runner.StartRunRequest(ctx, id, "feature_road_id",
    jobconfig.StartRequest{
        Input: json.RawMessage(`{}`),
        Parameters: map[string]json.RawMessage{
            "config": json.RawMessage(`"networks/config_jiangsu_2026.yaml"`),
            "road_area_table": json.RawMessage(`"hdroad_area0924"`),
            "feature_table": json.RawMessage(`"hdtraffic_ene"`),
        },
        Environment: map[string]string{
            "POSTGRES_HOST": dbHost,
            "POSTGRES_PORT": dbPort,
            "POSTGRES_USER": dbUser,
            "POSTGRES_PASSWORD": dbPassword,
        },
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
executables into the main image. Import
`github.com/xinge1982/docker-job-runner/jobrunnerclient` and
`github.com/xinge1982/docker-job-runner/jobconfig` through
your application's Go module dependency.

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
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o jobrunner ./cmd/jobrunner
install -d -m 0700 /srv/jobrunner/ssh/hostkeys
install -d -m 0700 /srv/jobrunner/ssh
install -d -o 10001 -g 10001 -m 0700 /srv/jobrunner/jobs
install -d -m 0755 /srv/jobrunner/programs /srv/jobrunner/networks
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
`mounts[*].source` in the task configuration also refers to paths on the
Docker host. Runtime environment variables travel over SSH stdin and are
injected into the child container at creation time.
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

## Production main application SSH credentials

Give the production main application its own SSH key, separate from the Windows
development key. Install its public key in the jobrunner container's
`authorized_keys`; keep the private key on the production application host.
`jobrunnerclient` invokes the system `ssh` command and reads that user's SSH
configuration. It does not load a public key as a client credential.

### Prepare the key and SSH configuration

On the production application host, create a dedicated directory and key:

```bash
install -d -m 0700 /data/sign/main-ssh
# Run only when this key does not already exist.
ssh-keygen -t ed25519 -N '' \
  -f /data/sign/main-ssh/jobrunner_ed25519 -C jobrunner-production
```

This example uses a key without a passphrase for unattended operation. If you
use a passphrase, provide an SSH agent with the key unlocked and make its socket
available to the application. Never commit the private key or copy it into an image.

Through an existing trusted connection to the jobrunner host, append the contents
of `jobrunner_ed25519.pub` as one line to the host file mounted as the runner's
`authorized_keys`. The repository's Compose example uses
`/srv/jobrunner/ssh/authorized_keys`; use your actual deployment path.

Create `/data/sign/main-ssh/config`:

```sshconfig
Host jobrunner-prod
    HostName YOUR_JOBRUNNER_HOST
    User jobrunner
    Port 2222
    IdentityFile ~/.ssh/jobrunner_ed25519
    IdentitiesOnly yes
    BatchMode yes
    StrictHostKeyChecking yes
```

Replace `YOUR_JOBRUNNER_HOST` with an address reachable from the main container.
If both containers share a Compose network, use the jobrunner service name and
its internal SSH port 22 instead of the published host port 2222.

### Verify the server host key

Obtain the jobrunner host key fingerprint through the trusted Linux host
connection, as described in the Windows setup section. On the production
application host, collect a candidate host key:

```bash
ssh-keyscan -t ed25519 -p 2222 YOUR_JOBRUNNER_HOST \
  > /data/sign/main-ssh/known_hosts.candidate
ssh-keygen -lf /data/sign/main-ssh/known_hosts.candidate
```

`ssh-keyscan` does not authenticate the server. Compare its fingerprint with
the trusted fingerprint. Only after they match, rename the candidate file to
`/data/sign/main-ssh/known_hosts`. Use the same hostname and port as the SSH
configuration; an internal service name on port 22 requires its own matching entry.

### Mount credentials into the main container

Assuming the main application user has home directory `/home/app`, add this
mount to its existing Compose service:

```yaml
services:
  main:
    volumes:
      - /data/sign/main-ssh:/home/app/.ssh:ro
```

Replace `/home/app` with the actual home directory of the user running the
application. For a root process this is usually `/root`. Set ownership to the
main container user's numeric UID and GID, and restrict file permissions:

```bash
# Replace 10001:10001 with the main application's actual UID:GID.
chown -R 10001:10001 /data/sign/main-ssh
chmod 700 /data/sign/main-ssh
chmod 600 /data/sign/main-ssh/config \
  /data/sign/main-ssh/jobrunner_ed25519 \
  /data/sign/main-ssh/known_hosts
```

The main image must include an OpenSSH client. For an Alpine-based image, add
`RUN apk add --no-cache openssh-client` to its Dockerfile. The main container
needs no Docker socket for SSH-backed jobrunner calls.

### Configure and verify the Go client

```go
runner := jobrunnerclient.Client{
    Binary:  "/usr/local/bin/jobrunner",
    Config:  "/etc/jobrunner/config.json", // Path inside the remote runner.
    Remote:  "jobrunner-prod",
    SSHPort: 2222, // Use 22 with the internal Compose service endpoint.
}
```

The client passes `SSHPort` explicitly, so keep it consistent with the endpoint.
Test from the main container as the same user that runs the application:

```bash
docker compose exec main ssh -T jobrunner-prod "id"
docker compose exec main ssh -T jobrunner-prod \
  "jobrunner describe --config /etc/jobrunner/config.json --type feature_road_id"
```

If the main application runs directly on a Linux host, place the SSH files in
the service user's `~/.ssh` directory instead of mounting them into a container.
The Go client configuration remains the same.

## Worker progress percentages

Workers may emit one complete JSON line on stdout or stderr with this exact
prefix. Flush each line (Python: `flush=True`) and avoid concurrent writers
splitting a progress line. Emit about once per second or each percentage change:

```text
JOBRUNNER_PROGRESS {"version":1,"percent":45,"phase":"process","completed":450,"total":1000,"message":"Processing features"}
```

`percent` is the overall percentage in the range 0 through 100, including
fractional values. `completed` and `total` are optional nonnegative integer
counts for the current phase; completed must not exceed total. Omit `percent`
when work cannot be estimated and supply `phase` or `message` instead. Use
stage weights in the worker to calculate overall progress. Preview and apply
are separate stages and each starts its own progress sequence. Do not include
credentials in progress messages or ordinary logs.

A Go worker can reuse the exported contract:

```go
import (
    "encoding/json"
    "fmt"

    "github.com/xinge1982/docker-job-runner/jobprogress"
)

func reportProgress(percent float64, phase, message string) error {
    b, err := json.Marshal(jobprogress.Progress{
        Version: 1,
        Percent: &percent,
        Phase: phase,
        Message: message,
    })
    if err != nil { return err }
    _, err = fmt.Printf("%s%s\n", jobprogress.Prefix, b)
    return err
}
```

The existing `status` command and `runner.GetStatus(ctx, id, stage)` now return
an optional `progress` object. The Go type is `*jobrunnerclient.Progress`:

```go
status, err := runner.GetStatus(ctx, id, "run")
if err != nil { return err }
if status.Progress != nil && status.Progress.Percent != nil {
    fmt.Printf("%.1f%% %s\n", *status.Progress.Percent, status.Progress.Message)
}
// status.ProgressError reports unavailable logs without failing state queries.
```

Each status query streams the full retained `docker logs --timestamps` output
with a 30-second log-read timeout, bounded memory, and a 64 KiB line limit.
Ordinary logs, malformed JSON, unsupported versions, and invalid values are
ignored. The latest valid Docker timestamp wins, even when stdout and stderr
arrive out of order. `updated_at` comes from Docker rather than the worker.
There is no fixed tail limit, so ordinary log output after a progress message
cannot hide it. This initial implementation does not cache progress or run a
background watcher: query cost grows with retained log size. For very large
logs, poll less frequently; incremental collection can be added later.
Docker log rotation can remove old progress messages, so periodically repeat
current progress during long phases. A logging driver must support reading logs.

A successful exited container (exit code zero) is normalized to 100 percent,
including workers without progress output. Running legacy workers omit progress.
Failures retain the last reported value, and a worker's 100 percent never changes
the container state. Always check `container_state` and validate business results.
On log-read failure, `progress_error` is set; available partial progress may still
be returned. Keep status responses in the main application's history if desired.
The existing archive includes progress lines in each stage's logs/docker.log.

Rebuild and redeploy the runner binary/container and update the main program's
module dependency to consume the added fields. No config.json change is required.
The main HTTP layer should explicitly expose these fields if it constructs its
own response rather than returning the complete Status value. Poll about every
2-3 seconds and stop after the container reaches a terminal state.
