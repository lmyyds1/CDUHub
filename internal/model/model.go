package model

type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Source struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	Kind         string    `json:"kind"`
	Group        string    `json:"group"`
	Channels     []Channel `json:"channels"`
	Status       string    `json:"status"`
	LastCrawled  string    `json:"lastCrawled"`
	Error        string    `json:"error"`
	ArticleCount int       `json:"articleCount"`
}

type Article struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Content    string `json:"content,omitempty"`
	URL        string `json:"url"`
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName"`
	SourceKind string `json:"sourceKind"`
	Channel    string `json:"channel"`
	Date       string `json:"date"`
	DateText   string `json:"dateText"`
	FetchedAt  string `json:"fetchedAt"`
}

type SyncStatus struct {
	Running    bool     `json:"running"`
	Phase      string   `json:"phase"`
	Current    string   `json:"current"`
	Completed  int      `json:"completed"`
	Total      int      `json:"total"`
	StartedAt  string   `json:"startedAt"`
	FinishedAt string   `json:"finishedAt"`
	Errors     []string `json:"errors"`
}

type Snapshot struct {
	Sources     []Source   `json:"sources"`
	Articles    []Article  `json:"articles"`
	Sync        SyncStatus `json:"sync"`
	LastUpdated string     `json:"lastUpdated"`
}

type SyncOptions struct {
	SourceID             string `json:"sourceId"`
	PagesPerChannel      int    `json:"pagesPerChannel"`
	MaxChannelsPerSource int    `json:"maxChannelsPerSource"`
}
