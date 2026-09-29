//go:build ignore

// Copy this file into the map-tool module and remove the build constraint above.
package jobapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xinge1982/docker-job-runner/jobconfig"
	"github.com/xinge1982/docker-job-runner/jobrunnerclient"
	"gorm.io/gorm"
	"map-tool/pg"
)

// Job and JobStage persist identifiers and execution state, never credentials.
type Job struct {
	ID             string    `gorm:"type:varchar(64);primaryKey" json:"id"`
	OwnerID        string    `gorm:"type:varchar(128);index;not null" json:"-"`
	TaskType       string    `gorm:"type:varchar(128);not null" json:"task_type"`
	Mode           string    `gorm:"type:varchar(16);not null" json:"mode"`
	ParametersJSON string    `gorm:"type:jsonb;not null" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (Job) TableName() string { return "job_records" }

type JobStage struct {
	JobID       string     `gorm:"type:varchar(64);primaryKey" json:"job_id"`
	Stage       string     `gorm:"type:varchar(16);primaryKey" json:"stage"`
	InputJSON   string     `gorm:"type:jsonb;not null" json:"-"`
	ContainerID string     `gorm:"type:varchar(128)" json:"container_id,omitempty"`
	Status      string     `gorm:"type:varchar(32);index;not null" json:"status"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	OOMKilled   bool       `json:"oom_killed"`
	ErrorText   string     `gorm:"type:text" json:"-"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	LastSyncAt  *time.Time `json:"last_sync_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (JobStage) TableName() string { return "job_stages" }

// Call once at application startup with the normal PostgreSQL GORM connection.
func InitJobTables(db *gorm.DB) error {
	return db.AutoMigrate(&Job{}, &JobStage{})
}

// Identity must derive the authenticated owner from your existing middleware.
type Identity func(*gin.Context) (ownerID string, ok bool)

// EnvironmentResolver reads the required names from server-side configuration.
// HTTP clients must never submit connection credentials in the request body.
type EnvironmentResolver func(*gin.Context, string, []jobconfig.EnvironmentVar) (map[string]string, error)

type Handler struct {
	Runner      jobrunnerclient.Client
	Identity    Identity
	Environment EnvironmentResolver
}

// EnvironmentFromProcess is a simple resolver for main services configured
// through process environment variables. Replace it with a secret store if needed.
func EnvironmentFromProcess(_ *gin.Context, _ string, specs []jobconfig.EnvironmentVar) (map[string]string, error) {
	values := make(map[string]string, len(specs))
	for _, spec := range specs {
		if value, ok := os.LookupEnv(spec.Name); ok {
			values[spec.Name] = value
		} else if spec.Required {
			return nil, errors.New("required worker environment is not configured: " + spec.Name)
		}
	}
	return values, nil
}

// Register attaches the handlers to a group, for example router.Group("/api").
func (h Handler) Register(r *gin.RouterGroup) {
	if h.Identity == nil || h.Environment == nil {
		panic("job API requires identity and environment resolvers")
	}
	r.GET("/job-types/:type", h.describe)
	r.POST("/jobs", h.create)
	r.GET("/jobs", h.list)
	r.GET("/jobs/:id", h.detail)
	r.POST("/jobs/:id/apply", h.apply)
	r.GET("/jobs/:id/stages/:stage", h.status)
	r.GET("/jobs/:id/stages/:stage/logs", h.logs)
	r.GET("/jobs/:id/stages/:stage/result", h.result)
}

type startBody struct {
	TaskType   string                     `json:"task_type"`
	Input      json.RawMessage            `json:"input"`
	Parameters map[string]json.RawMessage `json:"parameters"`
}

type applyBody struct {
	Input json.RawMessage `json:"input"`
}

func decodeBody(c *gin.Context, target interface{}) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("only one JSON document is allowed")
	}
	return nil
}

func (h Handler) owner(c *gin.Context) (string, bool) {
	owner, ok := h.Identity(c)
	if !ok || owner == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return "", false
	}
	return owner, true
}

func ownedJob(db *gorm.DB, id, owner string) (Job, error) {
	var job Job
	err := db.Where("id = ? AND owner_id = ?", id, owner).First(&job).Error
	return job, err
}

func ownedStage(db *gorm.DB, id, owner, stage string) (Job, JobStage, error) {
	job, err := ownedJob(db, id, owner)
	if err != nil {
		return Job{}, JobStage{}, err
	}
	var record JobStage
	err = db.Where("job_id = ? AND stage = ?", id, stage).First(&record).Error
	return job, record, err
}

func recordError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "job or stage not found"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "database operation failed"})
}

func (h Handler) describe(c *gin.Context) {
	if _, ok := h.owner(c); !ok {
		return
	}
	task, err := h.Runner.DescribeTask(c.Request.Context(), c.Param("type"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cannot describe task type"})
		return
	}
	// Do not expose host mount paths, program paths or fixed environment values.
	c.JSON(http.StatusOK, gin.H{
		"type": c.Param("type"), "mode": task.Mode,
		"parameters": task.Parameters, "environment_variables": task.EnvironmentVariables,
	})
}

func (h Handler) create(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	var body startBody
	if err := decodeBody(c, &body); err != nil || !json.Valid(body.Input) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid job request"})
		return
	}
	task, err := h.Runner.DescribeTask(c.Request.Context(), body.TaskType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown task type"})
		return
	}
	if _, err := task.FormatParameters(body.Parameters); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mode := task.Mode
	if mode == "" {
		mode = "staged"
	}
	if mode != "direct" && mode != "staged" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid task configuration"})
		return
	}
	env, err := h.Environment(c, body.TaskType, task.EnvironmentVariables)
	if err != nil || task.ValidateEnvironment(env) != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker environment is incomplete"})
		return
	}
	id, err := jobrunnerclient.NewJobID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot allocate job ID"})
		return
	}
	paramJSON, err := json.Marshal(body.Parameters)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid command parameters"})
		return
	}
	stage := "preview"
	if mode == "direct" {
		stage = "run"
	}
	job := Job{ID: id, OwnerID: owner, TaskType: body.TaskType, Mode: mode, ParametersJSON: string(paramJSON)}
	record := JobStage{JobID: id, Stage: stage, Status: "starting", InputJSON: string(body.Input)}
	db := pg.GormDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database unavailable"})
		return
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		return tx.Create(&record).Error
	}); err != nil {
		recordError(c, err)
		return
	}
	req := jobconfig.StartRequest{Input: body.Input, Parameters: body.Parameters, Environment: env}
	var containerID string
	if stage == "run" {
		containerID, err = h.Runner.StartRunRequest(c.Request.Context(), id, body.TaskType, req)
	} else {
		containerID, err = h.Runner.StartPreviewRequest(c.Request.Context(), id, body.TaskType, req)
	}
	if err != nil {
		// An SSH timeout can occur after Docker creates the container. Poll status
		// before retrying; the same job/stage cannot be submitted twice.
		_ = db.Model(&record).Updates(map[string]interface{}{"status": "unknown", "error_text": "runner start returned an error"}).Error
		c.JSON(http.StatusBadGateway, gin.H{"job_id": id, "stage": stage, "error": "runner response unknown; query job status"})
		return
	}
	if err := db.Model(&record).Updates(map[string]interface{}{"status": "created", "container_id": containerID}).Error; err != nil {
		recordError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": id, "stage": stage, "container_id": containerID})
}

func (h Handler) apply(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	var body applyBody
	if err := decodeBody(c, &body); err != nil || !json.Valid(body.Input) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid approved input"})
		return
	}
	db := pg.GormDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database unavailable"})
		return
	}
	job, _, err := ownedStage(db, c.Param("id"), owner, "preview")
	if err != nil {
		recordError(c, err)
		return
	}
	if job.Mode != "staged" {
		c.JSON(http.StatusConflict, gin.H{"error": "task does not support apply"})
		return
	}
	preview, err := h.Runner.GetStatus(c.Request.Context(), job.ID, "preview")
	if err != nil || preview.State.Status != "exited" || preview.State.ExitCode != 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "preview has not completed successfully"})
		return
	}
	if _, err := h.Runner.ReadResult(c.Request.Context(), preview, 32<<20); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "preview result is missing or invalid"})
		return
	}
	task, err := h.Runner.DescribeTask(c.Request.Context(), job.TaskType)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cannot describe task type"})
		return
	}
	var parameters map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job.ParametersJSON), &parameters); err != nil {
		recordError(c, err)
		return
	}
	if _, err := task.FormatParameters(parameters); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "task configuration changed; create a new job"})
		return
	}
	env, err := h.Environment(c, job.TaskType, task.EnvironmentVariables)
	if err != nil || task.ValidateEnvironment(env) != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker environment is incomplete"})
		return
	}
	record := JobStage{JobID: job.ID, Stage: "apply", Status: "starting", InputJSON: string(body.Input)}
	if err := db.Create(&record).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "apply stage already exists or cannot be created"})
		return
	}
	containerID, err := h.Runner.StartApplyRequest(c.Request.Context(), job.ID, job.TaskType,
		jobconfig.StartRequest{Input: body.Input, Parameters: parameters, Environment: env})
	if err != nil {
		_ = db.Model(&record).Updates(map[string]interface{}{"status": "unknown", "error_text": "runner start returned an error"}).Error
		c.JSON(http.StatusBadGateway, gin.H{"job_id": job.ID, "stage": "apply", "error": "runner response unknown; query job status"})
		return
	}
	if err := db.Model(&record).Updates(map[string]interface{}{"status": "created", "container_id": containerID}).Error; err != nil {
		recordError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "stage": "apply", "container_id": containerID})
}

func (h Handler) list(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	db := pg.GormDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database unavailable"})
		return
	}
	var jobs []Job
	if err := db.Where("owner_id = ?", owner).Order("created_at DESC").Limit(50).Find(&jobs).Error; err != nil {
		recordError(c, err)
		return
	}
	c.JSON(http.StatusOK, jobs)
}

func (h Handler) detail(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	db := pg.GormDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database unavailable"})
		return
	}
	job, err := ownedJob(db, c.Param("id"), owner)
	if err != nil {
		recordError(c, err)
		return
	}
	var stages []JobStage
	if err := db.Where("job_id = ?", job.ID).Order("created_at ASC").Find(&stages).Error; err != nil {
		recordError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job, "stages": stages})
}

func (h Handler) status(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	db := pg.GormDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database unavailable"})
		return
	}
	_, record, err := ownedStage(db, c.Param("id"), owner, c.Param("stage"))
	if err != nil {
		recordError(c, err)
		return
	}
	current, err := h.Runner.GetStatus(c.Request.Context(), record.JobID, record.Stage)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"stage": record, "error": "runner status unavailable"})
		return
	}
	now := time.Now()
	updates := map[string]interface{}{
		"status": current.State.Status, "container_id": current.ContainerID,
		"oom_killed": current.State.OOMKilled, "last_sync_at": now,
	}
	if current.State.Status == "exited" || current.State.Status == "dead" {
		updates["exit_code"] = current.State.ExitCode
	}
	if started, err := time.Parse(time.RFC3339Nano, current.State.StartedAt); err == nil {
		updates["started_at"] = started
	}
	if finished, err := time.Parse(time.RFC3339Nano, current.State.FinishedAt); err == nil {
		updates["finished_at"] = finished
	}
	if err := db.Model(&record).Updates(updates).Error; err != nil {
		recordError(c, err)
		return
	}
	if err := db.Where("job_id = ? AND stage = ?", record.JobID, record.Stage).First(&record).Error; err != nil {
		recordError(c, err)
		return
	}
	c.JSON(http.StatusOK, record)
}

func (h Handler) logs(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	db := pg.GormDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database unavailable"})
		return
	}
	_, record, err := ownedStage(db, c.Param("id"), owner, c.Param("stage"))
	if err != nil {
		recordError(c, err)
		return
	}
	tail := 200
	if value := c.Query("tail"); value != "" {
		tail, err = strconv.Atoi(value)
		if err != nil || tail < 0 || tail > 1000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tail must be between 0 and 1000"})
			return
		}
	}
	output, err := h.Runner.Logs(c.Request.Context(), record.JobID, record.Stage, tail)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "runner logs unavailable"})
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(output))
}

func (h Handler) result(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	db := pg.GormDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database unavailable"})
		return
	}
	_, record, err := ownedStage(db, c.Param("id"), owner, c.Param("stage"))
	if err != nil {
		recordError(c, err)
		return
	}
	state, err := h.Runner.GetStatus(c.Request.Context(), record.JobID, record.Stage)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "runner status unavailable"})
		return
	}
	if state.State.Status != "exited" || state.State.ExitCode != 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "stage has not completed successfully"})
		return
	}
	result, err := h.Runner.ReadResult(c.Request.Context(), state, 32<<20)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "runner result unavailable"})
		return
	}
	c.Data(http.StatusOK, "application/json", result)
}

// For apply, the worker must recheck candidate IDs and source versions.
