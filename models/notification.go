// Copyright 2016 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"fmt"
	"path"

	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/timeutil"

	"xorm.io/builder"
	"xorm.io/xorm"
)

type (
	// NotificationStatus is the status of the notification (read or unread)
	NotificationStatus uint8
	// NotificationSource is the source of the notification (issue, PR, commit, etc)
	NotificationSource uint8
)

const (
	// NotificationStatusUnread represents an unread notification
	NotificationStatusUnread NotificationStatus = iota + 1
	// NotificationStatusRead represents a read notification
	NotificationStatusRead
	// NotificationStatusPinned represents a pinned notification
	NotificationStatusPinned
)

const (
	// NotificationSourceIssue is a notification of an issue
	NotificationSourceIssue NotificationSource = iota + 1
	// NotificationSourcePullRequest is a notification of a pull request
	NotificationSourcePullRequest
	// NotificationSourceCommit is a notification of a commit
	NotificationSourceCommit
)

// Notification represents a notification
type Notification struct {
	ID     int64 `xorm:"pk autoincr"`
	UserID int64 `xorm:"INDEX NOT NULL"`
	RepoID int64 `xorm:"INDEX NOT NULL"`

	Status NotificationStatus `xorm:"SMALLINT INDEX NOT NULL"`
	Source NotificationSource `xorm:"SMALLINT INDEX NOT NULL"`

	IssueID   int64  `xorm:"INDEX NOT NULL"`
	CommitID  string `xorm:"INDEX"`
	CommentID int64

	UpdatedBy int64 `xorm:"INDEX NOT NULL"`

	Issue      *Issue      `xorm:"-"`
	Repository *Repository `xorm:"-"`
	Comment    *Comment    `xorm:"-"`
	User       *User       `xorm:"-"`

	CreatedUnix timeutil.TimeStamp `xorm:"created INDEX NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated INDEX NOT NULL"`
}

// FindNotificationOptions represent the filters for notifications. If an ID is 0 it will be ignored.
type FindNotificationOptions struct {
	UserID            int64
	RepoID            int64
	IssueID           int64
	Status            NotificationStatus
	UpdatedAfterUnix  int64
	UpdatedBeforeUnix int64
}

// ToCond will convert each condition into a xorm-Cond
func (opts *FindNotificationOptions) ToCond() builder.Cond {
	cond := builder.NewCond()
	if opts.UserID != 0 {
		cond = cond.And(builder.Eq{"notification.user_id": opts.UserID})
	}
	if opts.RepoID != 0 {
		cond = cond.And(builder.Eq{"notification.repo_id": opts.RepoID})
	}
	if opts.IssueID != 0 {
		cond = cond.And(builder.Eq{"notification.issue_id": opts.IssueID})
	}
	if opts.Status != 0 {
		cond = cond.And(builder.Eq{"notification.status": opts.Status})
	}
	if opts.UpdatedAfterUnix != 0 {
		cond = cond.And(builder.Gte{"notification.updated_unix": opts.UpdatedAfterUnix})
	}
	if opts.UpdatedBeforeUnix != 0 {
		cond = cond.And(builder.Lte{"notification.updated_unix": opts.UpdatedBeforeUnix})
	}
	return cond
}

// ToSession will convert the given options to a xorm Session by using the conditions from ToCond and joining with issue table if required
func (opts *FindNotificationOptions) ToSession(e Engine) *xorm.Session {
	return e.Where(opts.ToCond())
}

func getNotifications(e Engine, options FindNotificationOptions) (nl NotificationList, err error) {
	err = options.ToSession(e).OrderBy("notification.updated_unix DESC").Find(&nl)
	return
}

// GetNotifications returns all notifications that fit to the given options.
func GetNotifications(opts FindNotificationOptions) (NotificationList, error) {
	return getNotifications(x, opts)
}

// CreateOrUpdateIssueNotifications creates an issue notification
// for each watcher, or updates it if already exists
func CreateOrUpdateIssueNotifications(issueID, commentID int64, notificationAuthorID int64) error {
	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return err
	}

	if err := createOrUpdateIssueNotifications(sess, issueID, commentID, notificationAuthorID); err != nil {
		return err
	}

	return sess.Commit()
}

func createOrUpdateIssueNotifications(e Engine, issueID, commentID int64, notificationAuthorID int64) error {
	issueWatches, err := getIssueWatchers(e, issueID)
	if err != nil {
		return err
	}

	issue, err := getIssueByID(e, issueID)
	if err != nil {
		return err
	}

	watches, err := getWatchers(e, issue.RepoID)
	if err != nil {
		return err
	}

	notifications, err := getNotificationsByIssueID(e, issueID)
	if err != nil {
		return err
	}

	alreadyNotified := make(map[int64]struct{}, len(issueWatches)+len(watches))

	notifyUser := func(userID int64) error {
		// do not send notification for the own issuer/commenter
		if userID == notificationAuthorID {
			return nil
		}

		if _, ok := alreadyNotified[userID]; ok {
			return nil
		}
		alreadyNotified[userID] = struct{}{}

		if notificationExists(notifications, issue.ID, userID) {
			return updateIssueNotification(e, userID, issue.ID, commentID, notificationAuthorID)
		}
		return createIssueNotification(e, userID, issue, commentID, notificationAuthorID)
	}

	for _, issueWatch := range issueWatches {
		// ignore if user unwatched the issue
		if !issueWatch.IsWatching {
			alreadyNotified[issueWatch.UserID] = struct{}{}
			continue
		}

		if err := notifyUser(issueWatch.UserID); err != nil {
			return err
		}
	}

	err = issue.loadRepo(e)
	if err != nil {
		return err
	}

	for _, watch := range watches {
		issue.Repo.Units = nil
		if issue.IsPull && !issue.Repo.checkUnitUser(e, watch.UserID, false, UnitTypePullRequests) {
			continue
		}
		if !issue.IsPull && !issue.Repo.checkUnitUser(e, watch.UserID, false, UnitTypeIssues) {
			continue
		}

		if err := notifyUser(watch.UserID); err != nil {
			return err
		}
	}
	return nil
}

func getNotificationsByIssueID(e Engine, issueID int64) (notifications []*Notification, err error) {
	err = e.
		Where("issue_id = ?", issueID).
		Find(&notifications)
	return
}

func notificationExists(notifications []*Notification, issueID, userID int64) bool {
	for _, notification := range notifications {
		if notification.IssueID == issueID && notification.UserID == userID {
			return true
		}
	}

	return false
}

func createIssueNotification(e Engine, userID int64, issue *Issue, commentID, updatedByID int64) error {
	notification := &Notification{
		UserID:    userID,
		RepoID:    issue.RepoID,
		Status:    NotificationStatusUnread,
		IssueID:   issue.ID,
		CommentID: commentID,
		UpdatedBy: updatedByID,
	}

	if issue.IsPull {
		notification.Source = NotificationSourcePullRequest
	} else {
		notification.Source = NotificationSourceIssue
	}

	_, err := e.Insert(notification)
	return err
}

func updateIssueNotification(e Engine, userID, issueID, commentID, updatedByID int64) error {
	notification, err := getIssueNotification(e, userID, issueID)
	if err != nil {
		return err
	}

	// NOTICE: Only update comment id when the before notification on this issue is read, otherwise you may miss some old comments.
	// But we need update update_by so that the notification will be reorder
	var cols []string
	if notification.Status == NotificationStatusRead {
		notification.Status = NotificationStatusUnread
		notification.CommentID = commentID
		cols = []string{"status", "update_by", "comment_id"}
	} else {
		notification.UpdatedBy = updatedByID
		cols = []string{"update_by"}
	}

	_, err = e.ID(notification.ID).Cols(cols...).Update(notification)
	return err
}

func getIssueNotification(e Engine, userID, issueID int64) (*Notification, error) {
	notification := new(Notification)
	_, err := e.
		Where("user_id = ?", userID).
		And("issue_id = ?", issueID).
		Get(notification)
	return notification, err
}

// NotificationsForUser returns notifications for a given user and status
func NotificationsForUser(user *User, statuses []NotificationStatus, page, perPage int) (NotificationList, error) {
	return notificationsForUser(x, user, statuses, page, perPage)
}

func notificationsForUser(e Engine, user *User, statuses []NotificationStatus, page, perPage int) (notifications []*Notification, err error) {
	if len(statuses) == 0 {
		return
	}

	sess := e.
		Where("user_id = ?", user.ID).
		In("status", statuses).
		OrderBy("updated_unix DESC")

	if page > 0 && perPage > 0 {
		sess.Limit(perPage, (page-1)*perPage)
	}

	err = sess.Find(&notifications)
	return
}

// APIFormat converts a Notification to api.NotificationThread
func (n *Notification) APIFormat() *api.NotificationThread {
	result := &api.NotificationThread{
		ID:        n.ID,
		Unread:    !(n.Status == NotificationStatusRead || n.Status == NotificationStatusPinned),
		Pinned:    n.Status == NotificationStatusPinned,
		UpdatedAt: n.UpdatedUnix.AsTime(),
		URL:       n.APIURL(),
	}

	//since user only get notifications when he has access to use minimal access mode
	if n.Repository != nil {
		result.Repository = n.Repository.APIFormat(AccessModeRead)
	}

	//handle Subject
	switch n.Source {
	case NotificationSourceIssue:
		result.Subject = &api.NotificationSubject{Type: "Issue"}
		if n.Issue != nil {
			result.Subject.Title = n.Issue.Title
			result.Subject.URL = n.Issue.APIURL()
			comment, err := n.Issue.GetLastComment()
			if err == nil && comment != nil {
				result.Subject.LatestCommentURL = comment.APIURL()
			}
		}
	case NotificationSourcePullRequest:
		result.Subject = &api.NotificationSubject{Type: "Pull"}
		if n.Issue != nil {
			result.Subject.Title = n.Issue.Title
			result.Subject.URL = n.Issue.APIURL()
			comment, err := n.Issue.GetLastComment()
			if err == nil && comment != nil {
				result.Subject.LatestCommentURL = comment.APIURL()
			}
		}
	case NotificationSourceCommit:
		result.Subject = &api.NotificationSubject{
			Type:  "Commit",
			Title: n.CommitID,
		}
		//unused until now
	}

	return result
}

// LoadAttributes load Repo Issue User and Comment if not loaded
func (n *Notification) LoadAttributes() (err error) {
	return n.loadAttributes(x)
}

func (n *Notification) loadAttributes(e Engine) (err error) {
	if err = n.loadRepo(e); err != nil {
		return
	}
	if err = n.loadIssue(e); err != nil {
		return
	}
	if err = n.loadUser(e); err != nil {
		return
	}
	if err = n.loadComment(e); err != nil {
		return
	}
	return
}

func (n *Notification) loadRepo(e Engine) (err error) {
	if n.Repository == nil {
		n.Repository, err = getRepositoryByID(e, n.RepoID)
		if err != nil {
			return fmt.Errorf("getRepositoryByID [%d]: %v", n.RepoID, err)
		}
	}
	return nil
}

func (n *Notification) loadIssue(e Engine) (err error) {
	if n.Issue == nil {
		n.Issue, err = getIssueByID(e, n.IssueID)
		if err != nil {
			return fmt.Errorf("getIssueByID [%d]: %v", n.IssueID, err)
		}
		return n.Issue.loadAttributes(e)
	}
	return nil
}

func (n *Notification) loadComment(e Engine) (err error) {
	if n.Comment == nil && n.CommentID > 0 {
		n.Comment, err = GetCommentByID(n.CommentID)
		if err != nil {
			return fmt.Errorf("GetCommentByID [%d]: %v", n.CommentID, err)
		}
	}
	return nil
}

func (n *Notification) loadUser(e Engine) (err error) {
	if n.User == nil {
		n.User, err = getUserByID(e, n.UserID)
		if err != nil {
			return fmt.Errorf("getUserByID [%d]: %v", n.UserID, err)
		}
	}
	return nil
}

// GetRepo returns the repo of the notification
func (n *Notification) GetRepo() (*Repository, error) {
	return n.Repository, n.loadRepo(x)
}

// GetIssue returns the issue of the notification
func (n *Notification) GetIssue() (*Issue, error) {
	return n.Issue, n.loadIssue(x)
}

// HTMLURL formats a URL-string to the notification
func (n *Notification) HTMLURL() string {
	if n.Comment != nil {
		return n.Comment.HTMLURL()
	}
	return n.Issue.HTMLURL()
}

// APIURL formats a URL-string to the notification
func (n *Notification) APIURL() string {
	return setting.AppURL + path.Join("api/v1/notifications/threads", fmt.Sprintf("%d", n.ID))
}

// NotificationList contains a list of notifications
type NotificationList []*Notification

// APIFormat converts a NotificationList to api.NotificationThread list
func (nl NotificationList) APIFormat() []*api.NotificationThread {
	var result = make([]*api.NotificationThread, 0, len(nl))
	for _, n := range nl {
		result = append(result, n.APIFormat())
	}
	return result
}

// LoadAttributes load Repo Issue User and Comment if not loaded
func (nl NotificationList) LoadAttributes() (err error) {
	for i := 0; i < len(nl); i++ {
		err = nl[i].LoadAttributes()
		if err != nil {
			return
		}
	}
	return
}

func (nl NotificationList) getPendingRepoIDs() []int64 {
	var ids = make(map[int64]struct{}, len(nl))
	for _, notification := range nl {
		if notification.Repository != nil {
			continue
		}
		if _, ok := ids[notification.RepoID]; !ok {
			ids[notification.RepoID] = struct{}{}
		}
	}
	return keysInt64(ids)
}

// LoadRepos loads repositories from database
func (nl NotificationList) LoadRepos() (RepositoryList, error) {
	if len(nl) == 0 {
		return RepositoryList{}, nil
	}

	var repoIDs = nl.getPendingRepoIDs()
	var repos = make(map[int64]*Repository, len(repoIDs))
	var left = len(repoIDs)
	for left > 0 {
		var limit = defaultMaxInSize
		if left < limit {
			limit = left
		}
		rows, err := x.
			In("id", repoIDs[:limit]).
			Rows(new(Repository))
		if err != nil {
			return nil, err
		}

		for rows.Next() {
			var repo Repository
			err = rows.Scan(&repo)
			if err != nil {
				rows.Close()
				return nil, err
			}

			repos[repo.ID] = &repo
		}
		_ = rows.Close()

		left -= limit
		repoIDs = repoIDs[limit:]
	}

	var reposList = make(RepositoryList, 0, len(repoIDs))
	for _, notification := range nl {
		if notification.Repository == nil {
			notification.Repository = repos[notification.RepoID]
		}
		var found bool
		for _, r := range reposList {
			if r.ID == notification.Repository.ID {
				found = true
				break
			}
		}
		if !found {
			reposList = append(reposList, notification.Repository)
		}
	}
	return reposList, nil
}

func (nl NotificationList) getPendingIssueIDs() []int64 {
	var ids = make(map[int64]struct{}, len(nl))
	for _, notification := range nl {
		if notification.Issue != nil {
			continue
		}
		if _, ok := ids[notification.IssueID]; !ok {
			ids[notification.IssueID] = struct{}{}
		}
	}
	return keysInt64(ids)
}

// LoadIssues loads issues from database
func (nl NotificationList) LoadIssues() error {
	if len(nl) == 0 {
		return nil
	}

	var issueIDs = nl.getPendingIssueIDs()
	var issues = make(map[int64]*Issue, len(issueIDs))
	var left = len(issueIDs)
	for left > 0 {
		var limit = defaultMaxInSize
		if left < limit {
			limit = left
		}
		rows, err := x.
			In("id", issueIDs[:limit]).
			Rows(new(Issue))
		if err != nil {
			return err
		}

		for rows.Next() {
			var issue Issue
			err = rows.Scan(&issue)
			if err != nil {
				rows.Close()
				return err
			}

			issues[issue.ID] = &issue
		}
		_ = rows.Close()

		left -= limit
		issueIDs = issueIDs[limit:]
	}

	for _, notification := range nl {
		if notification.Issue == nil {
			notification.Issue = issues[notification.IssueID]
			notification.Issue.Repo = notification.Repository
		}
	}
	return nil
}

func (nl NotificationList) getPendingCommentIDs() []int64 {
	var ids = make(map[int64]struct{}, len(nl))
	for _, notification := range nl {
		if notification.CommentID == 0 || notification.Comment != nil {
			continue
		}
		if _, ok := ids[notification.CommentID]; !ok {
			ids[notification.CommentID] = struct{}{}
		}
	}
	return keysInt64(ids)
}

// LoadComments loads comments from database
func (nl NotificationList) LoadComments() error {
	if len(nl) == 0 {
		return nil
	}

	var commentIDs = nl.getPendingCommentIDs()
	var comments = make(map[int64]*Comment, len(commentIDs))
	var left = len(commentIDs)
	for left > 0 {
		var limit = defaultMaxInSize
		if left < limit {
			limit = left
		}
		rows, err := x.
			In("id", commentIDs[:limit]).
			Rows(new(Comment))
		if err != nil {
			return err
		}

		for rows.Next() {
			var comment Comment
			err = rows.Scan(&comment)
			if err != nil {
				rows.Close()
				return err
			}

			comments[comment.ID] = &comment
		}
		_ = rows.Close()

		left -= limit
		commentIDs = commentIDs[limit:]
	}

	for _, notification := range nl {
		if notification.CommentID > 0 && notification.Comment == nil && comments[notification.CommentID] != nil {
			notification.Comment = comments[notification.CommentID]
			notification.Comment.Issue = notification.Issue
		}
	}
	return nil
}

// GetNotificationCount returns the notification count for user
func GetNotificationCount(user *User, status NotificationStatus) (int64, error) {
	return getNotificationCount(x, user, status)
}

func getNotificationCount(e Engine, user *User, status NotificationStatus) (count int64, err error) {
	count, err = e.
		Where("user_id = ?", user.ID).
		And("status = ?", status).
		Count(&Notification{})
	return
}

func setNotificationStatusReadIfUnread(e Engine, userID, issueID int64) error {
	notification, err := getIssueNotification(e, userID, issueID)
	// ignore if not exists
	if err != nil {
		return nil
	}

	if notification.Status != NotificationStatusUnread {
		return nil
	}

	notification.Status = NotificationStatusRead

	_, err = e.ID(notification.ID).Update(notification)
	return err
}

// SetNotificationStatus change the notification status
func SetNotificationStatus(notificationID int64, user *User, status NotificationStatus) error {
	notification, err := getNotificationByID(x, notificationID)
	if err != nil {
		return err
	}

	if notification.UserID != user.ID {
		return fmt.Errorf("Can't change notification of another user: %d, %d", notification.UserID, user.ID)
	}

	notification.Status = status

	_, err = x.ID(notificationID).Update(notification)
	return err
}

// GetNotificationByID return notification by ID
func GetNotificationByID(notificationID int64) (*Notification, error) {
	return getNotificationByID(x, notificationID)
}

func getNotificationByID(e Engine, notificationID int64) (*Notification, error) {
	notification := new(Notification)
	ok, err := e.
		Where("id = ?", notificationID).
		Get(notification)

	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, ErrNotExist{ID: notificationID}
	}

	return notification, nil
}

// UpdateNotificationStatuses updates the statuses of all of a user's notifications that are of the currentStatus type to the desiredStatus
func UpdateNotificationStatuses(user *User, currentStatus NotificationStatus, desiredStatus NotificationStatus) error {
	n := &Notification{Status: desiredStatus, UpdatedBy: user.ID}
	_, err := x.
		Where("user_id = ? AND status = ?", user.ID, currentStatus).
		Cols("status", "updated_by", "updated_unix").
		Update(n)
	return err
}
