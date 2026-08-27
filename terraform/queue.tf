resource "google_cloud_tasks_queue" "webhook" {
  name     = "gcrunner-webhook"
  location = var.region

  retry_config {
    max_attempts       = -1
    max_retry_duration = "86400s"
    min_backoff        = "5s"
    max_backoff        = "300s"
    max_doublings      = 4
  }

  rate_limits {
    max_dispatches_per_second = 10
  }

  depends_on = [
    google_project_service.apis["cloudtasks.googleapis.com"],
  ]
}
