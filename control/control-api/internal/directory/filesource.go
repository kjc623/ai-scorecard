package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// FileSource reads a directory export from a JSON file. It is the lab's offline provider and the
// shape a customer with a non-Entra directory can export to: one document with a `users` array,
// each entry already reduced to the fields the sync stores.
//
// The mapping is explicit here rather than derived: each entry names its own `user_ref`, which is
// the value the device was configured with. This is the file source's half of the mapping the
// package comment states, and it is what lets the lab map the device simulator's pseudonymous refs
// without a live directory.
//
//	{
//	  "users": [
//	    { "user_ref": "u_8541", "directory_id": "…", "display_name": "Amara Okafor",
//	      "department": "Engineering", "population": "staff", "manager_ref": "…", "status": "active" }
//	  ]
//	}
//
// A missing `department` (or an explicit null/empty string) means the directory holds no department
// for that person, so the sync writes NULL and the read counts them unmapped. A missing `status`
// is 'active'.
type FileSource struct {
	Path string
}

// NewFileSource returns a source that reads Path.
func NewFileSource(path string) *FileSource { return &FileSource{Path: path} }

// Name implements Source.
func (s *FileSource) Name() string { return "file" }

type fileDocument struct {
	Users []fileUser `json:"users"`
}

type fileUser struct {
	UserRef        string `json:"user_ref"`
	DirectoryID    string `json:"directory_id"`
	DisplayName    string `json:"display_name"`
	Department     string `json:"department"`
	Population     string `json:"population"`
	ManagerRef     string `json:"manager_ref"`
	Status         string `json:"status"`
	AccountEnabled *bool  `json:"account_enabled"`
}

// List implements Source.
func (s *FileSource) List(_ context.Context) ([]User, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("directory: read file source: %w", err)
	}
	var doc fileDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("directory: parse file source %s: %w", s.Path, err)
	}
	users := make([]User, 0, len(doc.Users))
	for _, u := range doc.Users {
		status := u.Status
		if u.AccountEnabled != nil && u.Status == "" {
			if *u.AccountEnabled {
				status = "active"
			} else {
				status = "inactive"
			}
		}
		users = append(users, User{
			UserRef:     strings.TrimSpace(u.UserRef),
			DirectoryID: strings.TrimSpace(u.DirectoryID),
			DisplayName: strings.TrimSpace(u.DisplayName),
			Department:  strings.TrimSpace(u.Department),
			Population:  strings.TrimSpace(u.Population),
			ManagerRef:  strings.TrimSpace(u.ManagerRef),
			Status:      normalizeStatus(status),
		})
	}
	return users, nil
}
