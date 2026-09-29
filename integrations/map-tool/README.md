# Integrating jobrunner into map-tool

`jobapi.go` is a copyable Gin/GORM integration example. It has a
`//go:build ignore` header because `map-tool/pg` is part of the separate
`map-tool` module and is unavailable when testing this repository. Copy the
file into your main module, remove that one build constraint line, and adjust
the package name to match its destination directory.

The file imports `map-tool/pg` and
`github.com/xinge1982/docker-job-runner/jobrunnerclient`. Every HTTP handler
that uses the database obtains its connection with `pg.GormDB(c)`. At
application startup, run `jobapi.InitJobTables(db)` using your application's
regular PostgreSQL GORM connection. AutoMigrate creates `job_records` and
`job_stages`.

Register with the existing authentication middleware. The example below
expects that middleware to set `user_id` in the Gin context; adapt the
identity callback to your own authentication model:

```go
runner := jobrunnerclient.Client{
    Remote:  "jobrunner-dev",
    SSHPort: 2222,
    Binary:  "/usr/local/bin/jobrunner",
    Config:  "/etc/jobrunner/config.json",
}

handler := jobapi.Handler{
    Runner: runner,
    Identity: func(c *gin.Context) (string, bool) {
        id := c.GetString("user_id")
        return id, id != ""
    },
    Environment: jobapi.EnvironmentFromProcess,
}
handler.Register(router.Group("/api"))
```

Supply the variables declared under `environment_variables` to the main
service process. The HTTP API accepts task inputs and typed parameters, but
does not accept connection credentials from callers. The resolver may be
replaced with one that reads an authenticated tenant's secrets.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/job-types/:type` | Required parameters and environment variable names |
| POST | `/api/jobs` | Start `run` or `preview` according to task mode |
| GET | `/api/jobs` | List the current owner's latest 50 jobs |
| GET | `/api/jobs/:id` | Stored job and stages |
| POST | `/api/jobs/:id/apply` | Submit approved input after successful preview |
| GET | `/api/jobs/:id/stages/:stage` | Poll Docker and persist container state |
| GET | `/api/jobs/:id/stages/:stage/logs?tail=200` | Recent container output |
| GET | `/api/jobs/:id/stages/:stage/result` | Validated `result.json` after a successful exit |

Create a direct job:

```json
{
  "task_type": "feature_road_id",
  "input": {},
  "parameters": {
    "config": "networks/config_jiangsu_2026.yaml",
    "road_area_table": "hdroad_area0924",
    "feature_table": "hdtraffic_ene"
  }
}
```

For staged tasks, `POST /api/jobs` starts the preview. The caller inspects
its result and sends approved candidate IDs and the source version as the
`input` object to `POST /api/jobs/:id/apply`. The worker must validate those
IDs and source version again before changing business data. The HTTP handler
does not invent task-specific approval rules.

Status polling stores Docker state and exit code. The result endpoint reads
the task's `/job/output/result.json`; direct programs without that output
still support status and logs. The example does not schedule polling or retain
logs/results after child containers are removed. Keep containers until those
artifacts are captured by your production retention process.
