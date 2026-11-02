package google

import (
	"strings"
	"time"

	"echo-assistant/internal/tools"
)

// Writing contacts: save_contact and delete_contact of devices/atoms3r/src/services/google.cpp
// (the OAuth scope includes contacts).

// saveContact: a new contact, or more for an existing one (id from find_contact): a phone number,
// an e-mail address, a note. Existing numbers, addresses and notes stay.
func (a *Account) saveContact(args map[string]any, _ tools.Context) string {
	id, name, phone := tools.String(args, "id"), tools.String(args, "name"), tools.String(args, "phone")
	email, note := tools.String(args, "email"), tools.String(args, "note")
	kind := tools.String(args, "phone_type")
	if kind == "" {
		kind = "mobile"
	}
	if phone == "" && email == "" && note == "" && (id != "" || name == "") {
		return "Error: nothing to save (name for a new contact, phone, email or note)"
	}
	if id != "" && !strings.HasPrefix(id, "people/") {
		return "Error: id as in find_contact (people/...)"
	}
	ctx, cancel := callContext()
	defer cancel()
	person := map[string]any{}
	if id != "" { // the current entry with its etag: an update replaces whole fields
		err := a.get(ctx, peopleHost+"/v1/"+id+"?personFields=names,phoneNumbers,emailAddresses,biographies", &person)
		if StatusOf(err) == 404 {
			return "Error: no contact with this id (call find_contact first)"
		}
		if err != nil {
			return err.Error()
		}
	} else {
		person["names"] = []any{map[string]any{"unstructuredName": name}}
	}
	var fields []string
	if phone != "" {
		list, _ := person["phoneNumbers"].([]any)
		person["phoneNumbers"] = append(list, map[string]any{"value": phone, "type": kind})
		fields = append(fields, "phoneNumbers")
	}
	if email != "" {
		list, _ := person["emailAddresses"].([]any)
		person["emailAddresses"] = append(list, map[string]any{"value": email})
		fields = append(fields, "emailAddresses")
	}
	if note != "" { // one note per contact: appended to the existing one
		old := digS(person, "biographies", 0, "value")
		if old != "" {
			note = old + "\n" + note
		}
		person["biographies"] = []any{map[string]any{"value": note, "contentType": "TEXT_PLAIN"}}
		fields = append(fields, "biographies")
	}
	var saved map[string]any
	var err error
	if id != "" {
		err = a.JSON(ctx, "PATCH", peopleHost+"/v1/"+id+":updateContact?updatePersonFields="+strings.Join(fields, ",")+
			"&personFields=names,phoneNumbers,emailAddresses", person, &saved)
	} else {
		err = a.JSON(ctx, "POST", peopleHost+"/v1/people:createContact?personFields=names,phoneNumbers,emailAddresses",
			person, &saved)
	}
	if err != nil {
		return err.Error()
	}
	// So a caller with the new number is greeted by name: the directory is reloaded by Run,
	// between conversations (reloadDirectorySoon on the M5).
	a.dir.dueAt = time.Now()
	text := "Contact created: "
	if id != "" {
		text = "Contact updated: "
	}
	shown := digS(saved, "names", 0, "displayName")
	if shown == "" {
		shown = name
	}
	text += shown
	for _, entry := range digL(saved, "phoneNumbers") {
		text += ", phone " + digS(entry, "value")
	}
	for _, entry := range digL(saved, "emailAddresses") {
		text += ", mail " + digS(entry, "value")
	}
	return text + " [id " + digS(saved, "resourceName") + "]"
}

// AddContactWriteTools registers save_contact and delete_contact (the latter only for tests over
// the console). Call it where AddTools is called, if the account is enabled.
func (a *Account) AddContactWriteTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "save_contact",
		Description: "Saves a contact in the user's Google contacts, ONLY after you read out name and number (or address) and " +
			"the user agreed: a new one (id '', name required), or a phone number, e-mail address or note added to an " +
			"existing one (id from find_contact; search first so no duplicate is made). For 'speicher die Nummer vom " +
			"Handwerker'. Numbers as said, e.g. '0171 1234567'.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string", "description": "'' = new contact"},
                           "name": {"type": "string", "description": "full name or e.g. 'Elektriker Maier'"},
                           "phone": {"type": "string"},
                           "phone_type": {"type": "string", "enum": ["mobile", "home", "work", "other"]},
                           "email": {"type": "string"},
                           "note": {"type": "string", "description": "e.g. what they did for the user"}},
            "required": ["id", "name"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.saveContact})
	// Only for tests over the console: removes the test contact again.
	r.Add(tools.Tool{
		Name:        "delete_contact",
		Description: "Deletes a contact (id from find_contact).",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"}}, "required": ["id"]}`,
		Channels: tools.Console,
		Run: func(args map[string]any, _ tools.Context) string {
			id := tools.String(args, "id")
			if !strings.HasPrefix(id, "people/") {
				return "Error: id as in find_contact (people/...)"
			}
			ctx, cancel := callContext()
			defer cancel()
			if err := a.JSON(ctx, "DELETE", peopleHost+"/v1/"+id+":deleteContact", nil, nil); err != nil {
				return err.Error()
			}
			return "Deleted."
		}})
}
