package main

// Типы запросов/ответов API

type EnqueueRequest struct {
	URL      string `json:"url"`
	Priority *int   `json:"priority,omitempty"`
}

type EnqueueResponse struct {
	Enqueued bool   `json:"enqueued"`
	SiteID   int64  `json:"site_id"`
	URL      string `json:"url"`
	URLHash  string `json:"url_hash"`
	Message  string `json:"message,omitempty"`
}
