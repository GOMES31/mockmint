package admin

import (
	"github.com/mockmint/mockmint/internal/pkg"
)

type packageSummary struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	BasePath        string `json:"basePath"`
	Source          string `json:"source"`
	Origin          string `json:"origin"`
	Operations      int    `json:"operations"`
	AsyncOperations int    `json:"asyncOperations"`
	Proxy           string `json:"proxy,omitempty"`
}

type operationInfo struct {
	ID             string   `json:"id"`
	OperationID    string   `json:"operationId,omitempty"`
	Method         string   `json:"method"`
	Path           string   `json:"path"`
	Validation     string   `json:"validation"`
	Examples       []string `json:"examples"`
	DefaultExample string   `json:"defaultExample"`
	State          string   `json:"state,omitempty"` // "read notes"
	Warnings       []string `json:"warnings,omitempty"`
}

type asyncInfo struct {
	ID         string   `json:"id"`
	Action     string   `json:"action"`
	Channel    string   `json:"channel"`
	Queue      string   `json:"queue,omitempty"`
	Exchange   string   `json:"exchange,omitempty"`
	RoutingKey string   `json:"routingKey,omitempty"`
	Replies    bool     `json:"replies"`
	Examples   []string `json:"examples"`
	Schedule   string   `json:"schedule,omitempty"`
}

type packageDetail struct {
	packageSummary
	HTTP  []operationInfo `json:"http"`
	Async []asyncInfo     `json:"async"`
}

func summarize(p *pkg.Package, origin string) packageSummary {
	s := packageSummary{Name: p.Name, Version: p.Version, BasePath: p.BasePath, Source: p.Source, Origin: origin,
		Operations: len(p.Operations)}
	if s.BasePath == "" {
		s.BasePath = "/"
	}
	if p.Async != nil {
		s.AsyncOperations = len(p.Async.Operations)
	}
	if p.Proxy != nil {
		s.Proxy = p.Proxy.URL.Redacted()
	}
	return s
}

func describe(p *pkg.Package, origin string) packageDetail {
	d := packageDetail{packageSummary: summarize(p, origin), HTTP: []operationInfo{}, Async: []asyncInfo{}}
	for _, op := range p.Operations {
		oi := operationInfo{ID: op.ID, OperationID: op.OperationID, Method: op.Method, Path: op.Path,
			Validation: op.Validation, Examples: op.ExampleNames(), DefaultExample: op.DefaultExample, Warnings: op.Warnings}
		if op.State != nil {
			oi.State = op.State.Action + " " + op.State.Collection
		}
		d.HTTP = append(d.HTTP, oi)
	}
	if p.Async != nil {
		for _, op := range p.Async.Operations {
			ai := asyncInfo{ID: op.ID, Action: op.Action, Channel: op.Channel.ID, Queue: op.Queue,
				Exchange: op.Exchange, RoutingKey: op.RoutingKey, Replies: op.Replies != nil,
				Examples: pkg.ExampleNames(op.Templates)}
			if op.Replies != nil {
				ai.Examples = pkg.ExampleNames(op.Replies.Templates)
			}
			if op.Schedule != nil {
				ai.Schedule = op.Schedule.Interval.D().String()
			}
			d.Async = append(d.Async, ai)
		}
	}
	return d
}
