package main

import (
	"github.com/pquerna/otp/totp"
	"github.com/GeertJohan/yubigo"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/nmcclain/ldap"
	"net"
	"strings"
)

type configHandler struct {
	cfg *config
	yubikeyAuth *yubigo.YubiAuth
}

func newConfigHandler(cfg *config, yubikeyAuth *yubigo.YubiAuth) Backend {
	handler := configHandler{
		cfg: cfg,
		yubikeyAuth: yubikeyAuth }
	return handler
}

//
func (h configHandler) Bind(bindDN, bindSimplePw string, conn net.Conn) (resultCode ldap.LDAPResultCode, err error) {
	bindDN = strings.ToLower(bindDN)
	baseDN := strings.ToLower("," + h.cfg.Backend.BaseDN)
	log.Debug("Bind request as %s from %s", bindDN, conn.RemoteAddr().String())
	stats_frontend.Add("bind_reqs", 1)

	// parse the bindDN
	if !strings.HasSuffix(bindDN, baseDN) {
		log.Warning(fmt.Sprintf("Bind Error: BindDN %s not our BaseDN %s", bindDN, h.cfg.Backend.BaseDN))
		return ldap.LDAPResultInvalidCredentials, nil
	}
	parts := strings.Split(strings.TrimSuffix(bindDN, baseDN), ",")
	groupName := ""
	userName := ""
	if len(parts) == 1 {
		userName = strings.TrimPrefix(parts[0], h.cfg.Backend.NameAttr+"=")
	} else if len(parts) > 1 {
		userName = strings.TrimPrefix(parts[0], h.cfg.Backend.NameAttr+"=")
		groupName = strings.TrimPrefix(strings.TrimPrefix(parts[1], "cn="), "ou=")
	}
	// } else {
	// 	log.Warning(fmt.Sprintf("Bind Error: BindDN %s should have only one or two parts (has %d)", bindDN, len(parts)))
	// 	return ldap.LDAPResultInvalidCredentials, nil
	// }
	// find the user
	user := configUser{}
	found := false
	for _, u := range h.cfg.Users {
		if u.Name == userName {
			found = true
			user = u
		}
	}
	if !found {
		log.Warning(fmt.Sprintf("Bind Error: User %s not found.", user))
		return ldap.LDAPResultInvalidCredentials, nil
	}
	// find the group
	group := configGroup{}
	found = false
	for _, g := range h.cfg.Groups {
		if g.Name == groupName {
			found = true
			group = g
		}
	}
	if !found {
		log.Warning(fmt.Sprintf("Bind Error: Group %s not found.", group))
		return ldap.LDAPResultInvalidCredentials, nil
	}
	// validate group membership
	if user.PrimaryGroup != group.UnixID {
		log.Warning(fmt.Sprintf("Bind Error: User %s primary group is not %s.", userName, groupName))
		return ldap.LDAPResultInvalidCredentials, nil
	}
	
	validotp := false

	if len(user.Yubikey) == 0 && len(user.OTPSecret) == 0 {
		validotp = true
	}


	if len(user.Yubikey) > 0 && h.yubikeyAuth != nil {
		if len(bindSimplePw) > 44 {
			otp := bindSimplePw[len(bindSimplePw)-44:]
			yubikeyid := otp[0:12]
			bindSimplePw = bindSimplePw[:len(bindSimplePw)-44]

			if (user.Yubikey == yubikeyid) {
				_, ok, _ := h.yubikeyAuth.Verify(otp)

				if ok {
					validotp = true
				}
			}
		}
	}

	if len(user.OTPSecret) > 0 && !validotp {
		if len(bindSimplePw) > 6 {
			otp := bindSimplePw[len(bindSimplePw)-6:]
			bindSimplePw = bindSimplePw[:len(bindSimplePw)-6]

			validotp = totp.Validate(otp, user.OTPSecret)
		}	
	}

	if !validotp {
		log.Warning(fmt.Sprintf("Bind Error: invalid token as %s from %s", bindDN, conn.RemoteAddr().String()))
		return ldap.LDAPResultInvalidCredentials, nil
	}

	// finally, validate user's pw
	hash := sha256.New()
	hash.Write([]byte(bindSimplePw))

	if user.PassSHA256 != hex.EncodeToString(hash.Sum(nil)) {
		log.Warning(fmt.Sprintf("Bind Error: invalid credentials as %s from %s", bindDN, conn.RemoteAddr().String()))
		return ldap.LDAPResultInvalidCredentials, nil
	}
	stats_frontend.Add("bind_successes", 1)
	log.Debug("Bind success as %s from %s", bindDN, conn.RemoteAddr().String())
	return ldap.LDAPResultSuccess, nil
}

//
func (h configHandler) Search(bindDN string, searchReq ldap.SearchRequest, conn net.Conn) (result ldap.ServerSearchResult, err error) {
	bindDN = strings.ToLower(bindDN)
	baseDN := strings.ToLower("," + h.cfg.Backend.BaseDN)
	searchBaseDN := strings.ToLower(searchReq.BaseDN)
	log.Debug("Search request as %s from %s for %s", bindDN, conn.RemoteAddr().String(), searchReq.Filter)
	stats_frontend.Add("search_reqs", 1)

	// validate the user is authenticated and has appropriate access
	if len(bindDN) < 1 {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: Anonymous BindDN not allowed %s", bindDN)
	}
	if !strings.HasSuffix(bindDN, baseDN) {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: BindDN %s not in our BaseDN %s", bindDN, h.cfg.Backend.BaseDN)
	}
	if !strings.HasSuffix(searchBaseDN, h.cfg.Backend.BaseDN) {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: search BaseDN %s is not in our BaseDN %s", searchBaseDN, h.cfg.Backend.BaseDN)
	}
	// return all users in the config file - the LDAP library will filter results for us
	entries := []*ldap.Entry{}
	filterEntity, err := ldap.GetFilterObjectClass(searchReq.Filter)
	if err != nil {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultOperationsError}, fmt.Errorf("Search Error: error parsing filter: %s", searchReq.Filter)
	}
	switch filterEntity {
	default:
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultOperationsError}, fmt.Errorf("Search Error: unhandled filter type: %s [%s]", filterEntity, searchReq.Filter)
	case "posixgroup":
		for _, g := range h.cfg.Groups {
			attrs := []*ldap.EntryAttribute{}
			attrs = append(attrs, &ldap.EntryAttribute{"uid", []string{g.Name}})
			attrs = append(attrs, &ldap.EntryAttribute{"cn", []string{g.Name}})
			attrs = append(attrs, &ldap.EntryAttribute{"description", []string{fmt.Sprintf("%s via LDAP", g.Name)}})
			attrs = append(attrs, &ldap.EntryAttribute{"gidNumber", []string{fmt.Sprintf("%d", g.UnixID)}})
			attrs = append(attrs, &ldap.EntryAttribute{"objectClass", []string{"posixGroup"}})
			attrs = append(attrs, &ldap.EntryAttribute{"uniqueMember", h.getGroupMembers(g.UnixID)})
			attrs = append(attrs, &ldap.EntryAttribute{"memberUid", h.getGroupMemberIDs(g.UnixID)})
			dn := fmt.Sprintf("cn=%s,ou=groups,%s", g.Name, h.cfg.Backend.BaseDN)
			entries = append(entries, &ldap.Entry{dn, attrs})
		}
	case "posixaccount", "":
		for _, u := range h.cfg.Users {
			attrs := []*ldap.EntryAttribute{}
			attrs = append(attrs, &ldap.EntryAttribute{"cn", []string{u.Name}})
			attrs = append(attrs, &ldap.EntryAttribute{"uid", []string{u.Name}})

			if len(u.GivenName) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"givenName", []string{u.GivenName}})
			}

			if len(u.SN) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"sn", []string{u.SN}})
			}


			attrs = append(attrs, &ldap.EntryAttribute{"ou", []string{h.getGroupName(u.PrimaryGroup)}})
			attrs = append(attrs, &ldap.EntryAttribute{"uidNumber", []string{fmt.Sprintf("%d", u.UnixID)}})

			if (u.Disabled) {
				attrs = append(attrs, &ldap.EntryAttribute{"accountStatus", []string{"inactive"}})
			} else {
				attrs = append(attrs, &ldap.EntryAttribute{"accountStatus", []string{"active"}})
			}

			if len(u.Mail) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"mail", []string{u.Mail}})
			}

			attrs = append(attrs, &ldap.EntryAttribute{"objectClass", []string{"posixAccount"}})

			if len(u.LoginShell) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"loginShell", []string{u.LoginShell}})
			} else {
				attrs = append(attrs, &ldap.EntryAttribute{"loginShell", []string{"/bin/bash"}})
			}

			if len(u.Homedir) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"homeDirectory", []string{u.Homedir}})
			} else {
				attrs = append(attrs, &ldap.EntryAttribute{"homeDirectory", []string{"/home/" + u.Name}})
			}

			attrs = append(attrs, &ldap.EntryAttribute{"description", []string{fmt.Sprintf("%s via LDAP", u.Name)}})
			attrs = append(attrs, &ldap.EntryAttribute{"gecos", []string{fmt.Sprintf("%s", u.Name)}})
			attrs = append(attrs, &ldap.EntryAttribute{"gidNumber", []string{fmt.Sprintf("%d", u.PrimaryGroup)}})
			attrs = append(attrs, &ldap.EntryAttribute{"memberOf", h.getGroupDNs(u.OtherGroups)})
			if len(u.SSHKeys) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"sshPublicKey", u.SSHKeys})
				
				attrs = append(attrs, &ldap.EntryAttribute{"ipaSshPubKey", u.SSHKeys})
			}
			dn := fmt.Sprintf("cn=%s,ou=%s,%s", u.Name, h.getGroupName(u.PrimaryGroup), h.cfg.Backend.BaseDN)
			entries = append(entries, &ldap.Entry{dn, attrs})
		}
	}
	stats_frontend.Add("search_successes", 1)
	log.Debug("AP: Search OK: %s", searchReq.Filter)
	return ldap.ServerSearchResult{entries, []string{}, []ldap.Control{}, ldap.LDAPResultSuccess}, nil
}

//
func (h configHandler) Close(boundDn string, conn net.Conn) error {
	stats_frontend.Add("closes", 1)
	return nil
}

//
func (h configHandler) getGroupMembers(gid int) []string {
	members := make(map[string]bool)
	for _, u := range h.cfg.Users {
		if u.PrimaryGroup == gid {
			dn := fmt.Sprintf("cn=%s,ou=%s,%s", u.Name, h.getGroupName(u.PrimaryGroup), h.cfg.Backend.BaseDN)
			members[dn] = true
		} else {
			for _, othergid := range u.OtherGroups {
				if othergid == gid {
					dn := fmt.Sprintf("cn=%s,ou=%s,%s", u.Name, h.getGroupName(u.PrimaryGroup), h.cfg.Backend.BaseDN)
					members[dn] = true
				}
			}
		}
	}

	m := []string{}
	for k, _ := range members {
		m = append(m, k)
	}

	for _, g := range h.cfg.Groups {
		if gid == g.UnixID {
			for _, includegroupid := range g.IncludeGroups {
				if includegroupid != gid {
					m = append(m, h.getGroupMembers(includegroupid)...)
				}
			}
		}
	}

	return m
}

//
func (h configHandler) getGroupMemberIDs(gid int) []string {
	members := make(map[string]bool)
	for _, u := range h.cfg.Users {
		if u.PrimaryGroup == gid {
			members[u.Name] = true
		} else {
			for _, othergid := range u.OtherGroups {
				if othergid == gid {
					members[u.Name] = true
				}
			}
		}
	}
	
	m := []string{}
	for k, _ := range members {
		m = append(m, k)
	}
	
	for _, g := range h.cfg.Groups {
		if gid == g.UnixID {
			for _, includegroupid := range g.IncludeGroups {
				if includegroupid == gid {
					log.Warning(fmt.Sprintf("Group: %d - Ignoring myself as included group", includegroupid))
				} else {
					m = append(m, h.getGroupMemberIDs(includegroupid)...)
				}
			}
		}
	}
	
	return m
}

//
func (h configHandler) getGroupDNs(gids []int) []string {
	groups := make(map[string]bool)
	for _, gid := range gids {
		for _, g := range h.cfg.Groups {
			if g.UnixID == gid {
				dn := fmt.Sprintf("cn=%s,ou=groups,%s", g.Name, h.cfg.Backend.BaseDN)
				groups[dn] = true
			}
		}
	}
	g := []string{}
	for k, _ := range groups {
		g = append(g, k)
	}
	return g
}

//
func (h configHandler) getGroupName(gid int) string {
	for _, g := range h.cfg.Groups {
		// log.Debug(fmt.Sprintf("%s: %s", g.Name, g.OtherGroups))
		if g.UnixID == gid {
			return g.Name
		}
	}
	return ""
}
