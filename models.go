package main

import "github.com/hmsmart/runway/database/sqlcgen"

type User struct {
	userID      string
	telegramID  int64
	username    string
	firstname   string
	permissions []Permission
}

func NewUser(u sqlcgen.User) *User {
	newuser := &User{}
	if u.TgID == nil {
		return nil
	}
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
	newuser.permissions = make([]Permission, 0)
	if u.Active == true {
		newuser.permissions = append(newuser.permissions, PermissionActive)
	}
	if u.CanInvite == true {
		newuser.permissions = append(newuser.permissions, PermissionInvite)
	}
	return newuser
}
