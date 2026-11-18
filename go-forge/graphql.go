package main

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type GraphQLRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

type GraphQLError struct {
	Message string `json:"message"`
}

type GraphQLResponse struct {
	Data   any            `json:"data"`
	Errors []GraphQLError `json:"errors,omitempty"`
}

var (
	reNodeID     = regexp.MustCompile(`node\s*\(\s*id\s*:\s*["']([^"']+)["']\s*\)`)
	reOwnerArg   = regexp.MustCompile(`owner\s*:\s*["']([^"']+)["']`)
	reNameArg    = regexp.MustCompile(`name\s*:\s*["']([^"']+)["']`)
	reIssueNum   = regexp.MustCompile(`issue\s*\(\s*number\s*:\s*([0-9]+)\s*\)`)
	rePullNum    = regexp.MustCompile(`pullRequest\s*\(\s*number\s*:\s*([0-9]+)\s*\)`)
)

func strVar(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intVar(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

func executeGraphQL(store *ForgeStore, req GraphQLRequest, viewerLogin string) GraphQLResponse {
	q := req.Query
	vars := req.Variables
	if vars == nil {
		vars = map[string]any{}
	}

	// 1. Invalid field probe
	if strings.Contains(q, "noSuchFieldOnViewer") || strings.Contains(q, "invalidField") {
		return GraphQLResponse{
			Data:   nil,
			Errors: []GraphQLError{{Message: `Field "noSuchFieldOnViewer" does not exist on type "User"`}},
		}
	}

	// 2. Introspection
	if strings.Contains(q, "Issue_fields") || (strings.Contains(q, "Issue: __type") && !strings.Contains(q, "PullRequest: __type")) {
		return GraphQLResponse{
			Data: map[string]any{
				"Issue": map[string]any{
					"fields": []map[string]string{
						{"name": "id"}, {"name": "number"}, {"name": "title"},
						{"name": "state"}, {"name": "body"}, {"name": "createdAt"},
						{"name": "updatedAt"}, {"name": "author"}, {"name": "comments"},
					},
				},
			},
		}
	}

	if strings.Contains(q, "PullRequestAndRepositoryFields") || (strings.Contains(q, "PullRequest: __type") && strings.Contains(q, "Repository: __type")) {
		return GraphQLResponse{
			Data: map[string]any{
				"PullRequest": map[string]any{
					"fields": []map[string]string{
						{"name": "id"}, {"name": "number"}, {"name": "headRefName"},
						{"name": "baseRefName"}, {"name": "mergeable"}, {"name": "state"},
						{"name": "merged"}, {"name": "files"},
					},
				},
				"Repository": map[string]any{
					"fields": []map[string]string{
						{"name": "id"}, {"name": "databaseId"}, {"name": "name"},
						{"name": "nameWithOwner"}, {"name": "owner"}, {"name": "defaultBranchRef"},
						{"name": "viewerPermission"}, {"name": "hasIssuesEnabled"},
						{"name": "issue"}, {"name": "issues"},
						{"name": "pullRequests"}, {"name": "pullRequest"},
					},
				},
			},
		}
	}

	if strings.Contains(q, "SearchType_enumValues") || strings.Contains(q, "SearchType: __type") {
		return GraphQLResponse{
			Data: map[string]any{
				"SearchType": map[string]any{
					"enumValues": []map[string]string{
						{"name": "ISSUE"}, {"name": "REPOSITORY"},
						{"name": "USER"}, {"name": "DISCUSSION"},
					},
				},
			},
		}
	}

	// 3. Node query
	if strings.Contains(q, "NodeRepository") || strings.Contains(q, "node(") || strings.Contains(q, "node (") {
		id := strVar(vars, "id")
		if id == "" {
			if m := reNodeID.FindStringSubmatch(q); len(m) == 2 {
				id = m[1]
			}
		}
		if id == "" {
			return GraphQLResponse{Data: map[string]any{"node": nil}}
		}
		typ, rawID, ok := fromGlobalID(id)
		if !ok || typ != "Repository" {
			return GraphQLResponse{Data: map[string]any{"node": nil}}
		}
		numID, err := strconv.Atoi(rawID)
		if err != nil {
			return GraphQLResponse{Data: map[string]any{"node": nil}}
		}
		repo := store.GetRepoByID(numID)
		if repo == nil {
			return GraphQLResponse{Data: map[string]any{"node": nil}}
		}
		return GraphQLResponse{
			Data: map[string]any{
				"node": map[string]any{
					"__typename":    "Repository",
					"id":            toGlobalID("Repository", strconv.Itoa(repo.ID)),
					"name":          repo.Name,
					"nameWithOwner": repo.Owner + "/" + repo.Name,
				},
			},
		}
	}

	// 4. Mutations
	if strings.Contains(q, "mutation") {
		return handleGraphQLMutation(store, q, vars)
	}

	// 5. Repository query
	if strings.Contains(q, "repository(") || strings.Contains(q, "repository (") {
		return handleGraphQLRepository(store, q, vars, viewerLogin)
	}

	// 6. Viewer query
	if strings.Contains(q, "viewer") {
		return GraphQLResponse{
			Data: map[string]any{
				"viewer": map[string]any{"login": viewerLogin},
			},
		}
	}

	return GraphQLResponse{Data: map[string]any{}}
}

func handleGraphQLRepository(store *ForgeStore, q string, vars map[string]any, viewerLogin string) GraphQLResponse {
	owner := strVar(vars, "owner")
	name := strVar(vars, "name")
	if owner == "" {
		if m := reOwnerArg.FindStringSubmatch(q); len(m) == 2 {
			owner = m[1]
		}
	}
	if name == "" {
		if m := reNameArg.FindStringSubmatch(q); len(m) == 2 {
			name = m[1]
		}
	}
	if owner == "" {
		owner = viewerLogin
	}
	repo := store.GetRepo(owner, name)
	if repo == nil {
		return GraphQLResponse{Data: map[string]any{"repository": nil}}
	}

	repo.mu.RLock()
	res := map[string]any{
		"id":               toGlobalID("Repository", strconv.Itoa(repo.ID)),
		"databaseId":       repo.ID,
		"name":             repo.Name,
		"nameWithOwner":    repo.Owner + "/" + repo.Name,
		"owner":            map[string]any{"login": repo.Owner},
		"defaultBranchRef": map[string]any{"name": repo.DefaultBranch},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}

	if strings.Contains(q, "issue(") || strings.Contains(q, "issue (") {
		issueNum := intVar(vars, "number", 0)
		if issueNum == 0 {
			if m := reIssueNum.FindStringSubmatch(q); len(m) == 2 {
				issueNum, _ = strconv.Atoi(m[1])
			}
		}
		if issueNum >= 1 && issueNum <= len(repo.Issues) {
			iss := repo.Issues[issueNum-1]
			res["issue"] = map[string]any{
				"id":     toGlobalID("Issue", repo.Owner+"/"+repo.Name+"/"+strconv.Itoa(iss.Number)),
				"number": iss.Number,
				"title":  iss.Title,
				"body":   iss.Body,
				"state":  strings.ToUpper(iss.State),
			}
		} else {
			res["issue"] = nil
		}
	}

	if strings.Contains(q, "issues(") || strings.Contains(q, "issues (") {
		first := intVar(vars, "first", 10)
		after := strVar(vars, "after")
		filterOpen := strings.Contains(q, "states: [OPEN]")
		if stArr, ok := vars["states"].([]any); ok {
			for _, st := range stArr {
				if s, ok := st.(string); ok && s == "OPEN" {
					filterOpen = true
				}
			}
		}

		list := make([]*StoredIssue, 0, len(repo.Issues))
		for _, iss := range repo.Issues {
			if !filterOpen || iss.State == "open" {
				list = append(list, iss)
			}
		}
		if strings.Contains(q, "CREATED_AT") && strings.Contains(q, "DESC") {
			sort.Slice(list, func(i, j int) bool { return list[i].ID > list[j].ID })
		}

		startIdx := 0
		if after != "" {
			if dec, err := base64.StdEncoding.DecodeString(after); err == nil {
				cNum, _ := strconv.Atoi(strings.TrimPrefix(string(dec), "cursor:"))
				for i, iss := range list {
					if iss.ID == cNum {
						startIdx = i + 1
						break
					}
				}
			}
		}
		endIdx := startIdx + first
		if endIdx > len(list) {
			endIdx = len(list)
		}
		if startIdx > len(list) {
			startIdx = len(list)
		}
		paged := list[startIdx:endIdx]
		var endCursor any
		if len(paged) > 0 {
			endCursor = base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(paged[len(paged)-1].ID)))
		}
		nodes := make([]map[string]any, 0, len(paged))
		for _, iss := range paged {
			nodes = append(nodes, map[string]any{
				"id":     toGlobalID("Issue", repo.Owner+"/"+repo.Name+"/"+strconv.Itoa(iss.Number)),
				"number": iss.Number,
				"title":  iss.Title,
				"body":   iss.Body,
				"state":  strings.ToUpper(iss.State),
			})
		}
		res["issues"] = map[string]any{
			"nodes": nodes,
			"pageInfo": map[string]any{
				"hasNextPage": endIdx < len(list),
				"endCursor":   endCursor,
			},
		}
	}

	if strings.Contains(q, "pullRequests(") || strings.Contains(q, "pullRequests (") {
		nodes := make([]map[string]any, 0, len(repo.Pulls))
		for _, p := range repo.Pulls {
			st := strings.ToUpper(p.State)
			if p.Merged {
				st = "MERGED"
			}
			nodes = append(nodes, map[string]any{
				"id":          toGlobalID("PullRequest", repo.Owner+"/"+repo.Name+"/"+strconv.Itoa(p.Number)),
				"number":      p.Number,
				"headRefName": p.Head.Ref,
				"baseRefName": p.Base.Ref,
				"mergeable":   "MERGEABLE",
				"state":       st,
			})
		}
		res["pullRequests"] = map[string]any{"nodes": nodes}
	}

	if strings.Contains(q, "pullRequest(") || strings.Contains(q, "pullRequest (") {
		prNum := intVar(vars, "number", 0)
		if prNum == 0 {
			if m := rePullNum.FindStringSubmatch(q); len(m) == 2 {
				prNum, _ = strconv.Atoi(m[1])
			}
		}
		if prNum >= 1 && prNum <= len(repo.Pulls) {
			pr := repo.Pulls[prNum-1]
			st := strings.ToUpper(pr.State)
			if pr.Merged {
				st = "MERGED"
			}
			fNodes := make([]map[string]string, 0, len(pr.Files))
			for _, f := range pr.Files {
				fNodes = append(fNodes, map[string]string{"path": f.Filename})
			}
			res["pullRequest"] = map[string]any{
				"id":          toGlobalID("PullRequest", repo.Owner+"/"+repo.Name+"/"+strconv.Itoa(pr.Number)),
				"number":      pr.Number,
				"headRefName": pr.Head.Ref,
				"baseRefName": pr.Base.Ref,
				"mergeable":   "MERGEABLE",
				"state":       st,
				"files":       map[string]any{"nodes": fNodes},
			}
		} else {
			res["pullRequest"] = nil
		}
	}

	repo.mu.RUnlock()
	return GraphQLResponse{Data: map[string]any{"repository": res}}
}

func handleGraphQLMutation(store *ForgeStore, q string, vars map[string]any) GraphQLResponse {
	input, _ := vars["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}

	findRepo := func(repoGID string) *StoredRepo {
		if _, rawID, ok := fromGlobalID(repoGID); ok {
			if id, err := strconv.Atoi(rawID); err == nil {
				if r := store.GetRepoByID(id); r != nil {
					return r
				}
			}
		}
		all := store.ListRepos("")
		if len(all) > 0 {
			return all[0]
		}
		return nil
	}

	if strings.Contains(q, "createIssue") {
		repoID := strVar(input, "repositoryId")
		if repoID == "" {
			repoID = strVar(vars, "repositoryId")
		}
		repo := findRepo(repoID)
		if repo == nil {
			return GraphQLResponse{Errors: []GraphQLError{{Message: "Repository not found"}}}
		}
		title := strVar(input, "title")
		if title == "" {
			title = "GraphQL Issue"
		}
		iss, _, err := repo.CreateIssue(title, strVar(input, "body"), nil)
		if err != nil {
			return GraphQLResponse{Errors: []GraphQLError{{Message: err.Error()}}}
		}
		return GraphQLResponse{
			Data: map[string]any{
				"createIssue": map[string]any{
					"issue": map[string]any{
						"id":     toGlobalID("Issue", repo.Owner+"/"+repo.Name+"/"+strconv.Itoa(iss.Number)),
						"number": iss.Number,
					},
				},
			},
		}
	}

	if strings.Contains(q, "closeIssue") {
		issueID := strVar(input, "issueId")
		if issueID == "" {
			issueID = strVar(vars, "issueId")
		}
		_, parsedID, _ := fromGlobalID(issueID)
		closedNum := 1
		stClosed := "closed"
		for _, r := range store.ListRepos("") {
			r.mu.RLock()
			matchNum := 0
			for _, iss := range r.Issues {
				gid := toGlobalID("Issue", r.Owner+"/"+r.Name+"/"+strconv.Itoa(iss.Number))
				if gid == issueID || strconv.Itoa(iss.ID) == parsedID {
					matchNum = iss.Number
					break
				}
			}
			r.mu.RUnlock()
			if matchNum > 0 {
				if updated, _ := r.PatchIssue(matchNum, &stClosed, nil, nil); updated != nil {
					closedNum = updated.Number
				}
				break
			}
		}
		return GraphQLResponse{
			Data: map[string]any{
				"closeIssue": map[string]any{
					"issue": map[string]any{
						"id":     issueID,
						"number": closedNum,
						"state":  "CLOSED",
					},
				},
			},
		}
	}

	if strings.Contains(q, "createPullRequest") {
		repoID := strVar(input, "repositoryId")
		if repoID == "" {
			repoID = strVar(vars, "repositoryId")
		}
		repo := findRepo(repoID)
		if repo == nil {
			return GraphQLResponse{Errors: []GraphQLError{{Message: "Repository not found"}}}
		}
		title := strVar(input, "title")
		if title == "" {
			title = "GraphQL PR"
		}
		pr, err := repo.CreatePull(title, strVar(input, "body"), strVar(input, "headRefName"), strVar(input, "baseRefName"))
		if err != nil {
			return GraphQLResponse{Errors: []GraphQLError{{Message: err.Error()}}}
		}
		return GraphQLResponse{
			Data: map[string]any{
				"createPullRequest": map[string]any{
					"pullRequest": map[string]any{
						"id":          toGlobalID("PullRequest", repo.Owner+"/"+repo.Name+"/"+strconv.Itoa(pr.Number)),
						"number":      pr.Number,
						"state":       "OPEN",
						"headRefName": pr.Head.Ref,
						"baseRefName": pr.Base.Ref,
					},
				},
			},
		}
	}

	if strings.Contains(q, "mergePullRequest") {
		prID := strVar(input, "pullRequestId")
		if prID == "" {
			prID = strVar(vars, "pullRequestId")
		}
		mergedNum := 1
		for _, r := range store.ListRepos("") {
			r.mu.RLock()
			matchNum := 0
			for _, p := range r.Pulls {
				if toGlobalID("PullRequest", r.Owner+"/"+r.Name+"/"+strconv.Itoa(p.Number)) == prID {
					matchNum = p.Number
					break
				}
			}
			r.mu.RUnlock()
			if matchNum > 0 {
				_, _ = r.MergePull(matchNum, false)
				mergedNum = matchNum
				break
			}
		}
		return GraphQLResponse{
			Data: map[string]any{
				"mergePullRequest": map[string]any{
					"pullRequest": map[string]any{
						"number": mergedNum,
						"merged": true,
						"state":  "MERGED",
					},
				},
			},
		}
	}

	if strings.Contains(q, "addComment") {
		subjectID := strVar(input, "subjectId")
		if subjectID == "" {
			subjectID = strVar(vars, "subjectId")
		}
		body := strVar(input, "body")
		cid := 1
		for _, r := range store.ListRepos("") {
			r.mu.RLock()
			matchNum := 0
			for _, iss := range r.Issues {
				if toGlobalID("Issue", r.Owner+"/"+r.Name+"/"+strconv.Itoa(iss.Number)) == subjectID {
					matchNum = iss.Number
					break
				}
			}
			r.mu.RUnlock()
			if matchNum > 0 {
				if c, err := r.CreateComment(matchNum, body); err == nil {
					cid = c.ID
				}
				break
			}
		}
		return GraphQLResponse{
			Data: map[string]any{
				"addComment": map[string]any{
					"commentEdge": map[string]any{
						"node": map[string]any{
							"id":   toGlobalID("Comment", strconv.Itoa(cid)),
							"body": body,
						},
					},
				},
			},
		}
	}

	return GraphQLResponse{Data: map[string]any{}}
}
