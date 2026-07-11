package domains

import (
	"slices"

	"github.com/hmsmart/runway/database/sqlcgen"
)

type Permission string

const (
	PermissionInvite       Permission = "invite"
	PermissionActive       Permission = "active"
	PermissionUnregistered Permission = "unregistered"
)

type User struct {
	userID        string
	telegramID    int64
	username      string
	firstname     string
	discretionary *float64
	reportTime    *string
	permissions   []Permission
}

// NewUser adapts a database row to the domain model. A row with no Telegram
// ID is an unregistered sender, represented as a nil *User.
func NewUser(u sqlcgen.User) *User {
	if u.TgID == nil {
		return nil
	}
	newuser := &User{}
	newuser.userID = u.ID
	newuser.telegramID = *u.TgID
	if u.TgFirstName == nil {
		newuser.firstname = "unknown user"
	} else {
		newuser.firstname = *u.TgFirstName
	}
	if u.TgUsername == nil {
		newuser.username = "unknown username"
	} else {
		newuser.username = *u.TgUsername
	}
	newuser.discretionary = u.DiscretionaryMonthly
	newuser.reportTime = u.ReportTime
	newuser.permissions = make([]Permission, 0)
	if u.Active == true {
		newuser.permissions = append(newuser.permissions, PermissionActive)
	}
	if u.CanInvite == true {
		newuser.permissions = append(newuser.permissions, PermissionInvite)
	}
	return newuser
}

// Has reports whether the user holds the permission. It is safe on a nil
// receiver: an unregistered sender holds only PermissionUnregistered.
func (u *User) Has(p Permission) bool {
	if u == nil {
		return p == PermissionUnregistered
	}
	return slices.Contains(u.permissions, p)
}

// Discretionary returns the user's monthly discretionary budget, or nil if
// they haven't set one yet. Setup requires it before an account can be linked.
func (u *User) Discretionary() *float64 { return u.discretionary }

// ReportTime returns the user's scheduled daily report time as a zero-padded
// 24h "HH:MM", or nil if they haven't scheduled one.
func (u *User) ReportTime() *string { return u.reportTime }

func (u *User) ID() string        { return u.userID }
func (u *User) TelegramID() int64 { return u.telegramID }
func (u *User) Username() string  { return u.username }
func (u *User) FirstName() string { return u.firstname }
