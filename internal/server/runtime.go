package server

import (
	"net/http"
	"time"
)

func (s *Server) runtimeStatus(w http.ResponseWriter) {
	items, err := s.Store.RuntimeStatuses()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "运行状态读取失败")
		return
	}
	counts, err := s.Store.JobStatusCounts()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "后台任务状态读取失败")
		return
	}
	failedJobs, err := s.Store.FailedJobs(5)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "失败任务读取失败")
		return
	}
	overall := "ok"
	for i := range items {
		if items[i].Status == "error" {
			overall = "error"
			break
		}
		if items[i].Status == "running" {
			overall = "working"
		}
	}
	s.json(w, http.StatusOK, map[string]any{
		"success":     true,
		"overall":     overall,
		"components":  items,
		"job_counts":  counts,
		"failed_jobs": failedJobs,
		"server_time": time.Now().Unix(),
	})
}

func (s *Server) retryRuntimeJob(w http.ResponseWriter, data map[string]any) {
	jobID := stringValue(data["jobId"])
	if jobID == "" {
		s.error(w, http.StatusBadRequest, "任务编号不能为空")
		return
	}
	requeued, err := s.Store.RequeueFailedJob(jobID)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "任务重新入队失败")
		return
	}
	if !requeued {
		s.error(w, http.StatusConflict, "任务不存在或已不再是失败状态")
		return
	}
	s.json(w, http.StatusOK, map[string]any{"success": true})
}
