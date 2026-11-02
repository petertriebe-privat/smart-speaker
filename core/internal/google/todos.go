package google

import (
	"fmt"
	"strconv"
	"strings"

	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// The user's to-do lists in Google Tasks: what they see in the Google Tasks app on their phone
// (scope tasks). Tools (the admin's account: Private): list_todos, add_todo, change_todo. A task's
// id for the model is "<list id>/<task id>". The shopping list stays in Home Assistant. As
// devices/atoms3r/src/services/todos.cpp.

const (
	tasksBase = "https://tasks.googleapis.com/tasks/v1"
	maxTasks  = 100 // per list
)

type taskList struct{ ID, Title string }

func (a *Account) tasks(method, path string, in, out any) error {
	ctx, cancel := callContext()
	defer cancel()
	if method == "GET" {
		return a.get(ctx, tasksBase+path, out)
	}
	return a.JSON(ctx, method, tasksBase+path, in, out)
}

func (a *Account) taskLists() ([]taskList, error) {
	var answer map[string]any
	if err := a.tasks("GET", "/users/@me/lists?maxResults=100", nil, &answer); err != nil {
		return nil, err
	}
	var found []taskList
	for _, l := range digL(answer, "items") {
		found = append(found, taskList{digS(l, "id"), digS(l, "title")})
	}
	return found, nil
}

// findList: the list named name (case-insensitive; "" = the first one, Google's default). make:
// made when missing.
func (a *Account) findList(name string, make bool) (taskList, error) {
	all, err := a.taskLists()
	if err != nil {
		return taskList{}, err
	}
	for _, candidate := range all {
		if name == "" || strings.EqualFold(candidate.Title, name) {
			return candidate, nil
		}
	}
	if name == "" || !make {
		var names []string
		for _, candidate := range all {
			names = append(names, "\""+candidate.Title+"\"")
		}
		return taskList{}, fmt.Errorf("Error: no list \"%s\" (lists: %s)", name, strings.Join(names, ", "))
	}
	var answer map[string]any
	if err := a.tasks("POST", "/users/@me/lists", map[string]any{"title": name}, &answer); err != nil {
		return taskList{}, err
	}
	return taskList{digS(answer, "id"), name}, nil
}

// dueValue: "YYYY-MM-DD" -> Google's due value (Tasks keeps only the date). "" if malformed.
func dueValue(date string) string {
	d, ok := scanInts(date, '-', 4, 2, 2)
	if !ok || d[1] < 1 || d[1] > 12 || d[2] < 1 || d[2] > 31 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02dT00:00:00.000Z", d[0], d[1], d[2])
}

func describeTask(task map[string]any, listID string) string {
	title := digS(task, "title")
	if title == "" {
		title = "(no title)"
	}
	line := "- " + title
	if due := digS(task, "due"); len(due) >= 10 {
		line += " | due " + due[8:10] + "." + due[5:7] + "." + due[0:4]
	}
	if digS(task, "status") == "completed" {
		line += " | done"
	}
	if notes := digS(task, "notes"); notes != "" {
		line += " | note: " + textutil.Prefix(strings.ReplaceAll(notes, "\n", " "), 150)
	}
	return line + " [id " + listID + "/" + digS(task, "id") + "]\n"
}

// splitTaskID: "<list>/<task>" -> the two ids. False if malformed.
func splitTaskID(id string) (list, task string, ok bool) {
	slash := strings.IndexByte(id, '/')
	if slash <= 0 || slash == len(id)-1 {
		return "", "", false
	}
	return id[:slash], id[slash+1:], true
}

func (a *Account) listTodos(args map[string]any, _ tools.Context) string {
	name := tools.String(args, "list")
	done := tools.Bool(args, "include_done", false)
	var selected []taskList
	if name != "" {
		list, err := a.findList(name, false)
		if err != nil {
			return err.Error()
		}
		selected = []taskList{list}
	} else {
		var err error
		if selected, err = a.taskLists(); err != nil {
			return err.Error()
		}
	}
	text := ""
	for _, list := range selected {
		path := "/lists/" + enc(list.ID) + "/tasks?maxResults=" + strconv.Itoa(maxTasks) + "&showCompleted="
		if done {
			path += "true&showHidden=true"
		} else {
			path += "false"
		}
		var answer map[string]any
		if err := a.tasks("GET", path, nil, &answer); err != nil {
			return err.Error()
		}
		items := digL(answer, "items")
		text += "List \"" + list.Title + "\""
		if len(items) > 0 {
			text += ":\n"
		} else {
			text += ": empty\n"
		}
		for _, task := range items {
			m, _ := task.(map[string]any)
			text += describeTask(m, list.ID)
		}
	}
	if text == "" {
		return "No to-do lists."
	}
	return text
}

func (a *Account) addTodo(args map[string]any, _ tools.Context) string {
	a.refreshAgenda() // the agenda of the next conversation shows the change
	title := tools.String(args, "title")
	if title == "" {
		return "Error: title missing"
	}
	listName, _ := argString(args, "list")
	list, err := a.findList(listName, true)
	if err != nil {
		return err.Error()
	}
	task := map[string]any{"title": title}
	if notes, _ := argString(args, "notes"); notes != "" {
		task["notes"] = notes
	}
	if due, _ := argString(args, "due"); due != "" {
		value := dueValue(due)
		if value == "" {
			return "Error: due as YYYY-MM-DD"
		}
		task["due"] = value
	}
	var answer map[string]any
	if err := a.tasks("POST", "/lists/"+enc(list.ID)+"/tasks", task, &answer); err != nil {
		return err.Error()
	}
	return "Added to \"" + list.Title + "\":\n" + describeTask(answer, list.ID)
}

func (a *Account) changeTodo(args map[string]any, _ tools.Context) string {
	a.refreshAgenda() // the agenda of the next conversation shows the change
	id, _ := argString(args, "id")
	listID, taskID, ok := splitTaskID(id)
	if !ok {
		return "Error: id as in list_todos (list/task)"
	}
	action, _ := argString(args, "action")
	path := "/lists/" + enc(listID) + "/tasks/" + enc(taskID)
	if action == "delete" {
		err := a.tasks("DELETE", path, nil, nil)
		if StatusOf(err) == 404 {
			return "Error: no such task (call list_todos first)"
		}
		if err != nil {
			return err.Error()
		}
		return "Deleted."
	}
	change := map[string]any{}
	switch action {
	case "complete":
		change["status"] = "completed"
	case "reopen":
		change["status"] = "needsAction"
		change["completed"] = nil
	case "edit":
		for _, key := range []string{"title", "notes"} {
			if s, ok := argString(args, key); ok {
				change[key] = s
			}
		}
		if due, ok := argString(args, "due"); ok {
			value := dueValue(due)
			if due != "" && value == "" {
				return "Error: due as YYYY-MM-DD, '' removes it"
			}
			if value != "" {
				change["due"] = value
			} else {
				change["due"] = nil
			}
		}
		if len(change) == 0 {
			return "Error: nothing to change given"
		}
	default:
		return "Error: action is complete, reopen, edit or delete"
	}
	var answer map[string]any
	err := a.tasks("PATCH", path, change, &answer)
	if StatusOf(err) == 404 {
		return "Error: no such task (call list_todos first)"
	}
	if err != nil {
		return err.Error()
	}
	// Google keeps deleted tasks a while and lets them be changed: they are gone for the user.
	if digB(answer, "deleted") {
		return "Error: this task was deleted"
	}
	prefix := "Changed:\n"
	switch action {
	case "complete":
		prefix = "Done:\n"
	case "reopen":
		prefix = "Open again:\n"
	}
	return prefix + describeTask(answer, listID)
}

// Todo is an open to-do with a due date, for the agenda.
type Todo struct {
	ID    string // "<list id>/<task id>"
	Title string
	Due   string // YYYY-MM-DD
	List  string // its list's title when there are several lists, else ""
}

// DueTodos: the open to-dos of all lists due on lastDay (YYYY-MM-DD) or earlier, overdue ones
// included. Blocks for seconds (one request per list).
func (a *Account) DueTodos(lastDay string) ([]Todo, error) {
	dueMax := dueValue(lastDay)
	if dueMax == "" {
		return nil, fmt.Errorf("Error: lastDay as YYYY-MM-DD")
	}
	all, err := a.taskLists()
	if err != nil {
		return nil, err
	}
	var out []Todo
	for _, list := range all {
		// dueMax is exclusive in Google's API: the day after lastDay would need date arithmetic,
		// so the end of lastDay ("T23:59:59").
		endOfDay := strings.Replace(dueMax, "T00:00:00", "T23:59:59", 1)
		path := "/lists/" + enc(list.ID) + "/tasks?maxResults=" + strconv.Itoa(maxTasks) +
			"&showCompleted=false&dueMax=" + enc(endOfDay)
		var answer map[string]any
		if err := a.tasks("GET", path, nil, &answer); err != nil {
			return nil, err
		}
		for _, task := range digL(answer, "items") {
			date := sub(digS(task, "due"), 0, 10)
			if len(date) < 10 {
				continue
			}
			title := digS(task, "title")
			if title == "" {
				title = "(no title)"
			}
			t := Todo{ID: list.ID + "/" + digS(task, "id"), Title: title, Due: date}
			if len(all) > 1 {
				t.List = list.Title
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// AllOpenTodos: the open to-dos of every list except skip (The assistant's own), dated or not, for
// matching mails against them (#71). Blocks for seconds (one request per list).
func (a *Account) AllOpenTodos(skip string) ([]Todo, error) {
	all, err := a.taskLists()
	if err != nil {
		return nil, err
	}
	var out []Todo
	for _, list := range all {
		if strings.EqualFold(list.Title, skip) {
			continue
		}
		var answer map[string]any
		if err := a.tasks("GET", "/lists/"+enc(list.ID)+"/tasks?maxResults="+strconv.Itoa(maxTasks)+"&showCompleted=false", nil, &answer); err != nil {
			return nil, err
		}
		for _, task := range digL(answer, "items") {
			if title := digS(task, "title"); title != "" {
				out = append(out, Todo{ID: list.ID + "/" + digS(task, "id"), Title: title, Due: sub(digS(task, "due"), 0, 10), List: list.Title})
			}
		}
	}
	return out, nil
}

// AddOwnTodo: a to-do in the list named list (made when missing): The assistant's own list of what it
// inferred (#71). Returns its id "<list>/<task>".
func (a *Account) AddOwnTodo(list, title, due, notes string) (string, error) {
	l, err := a.findList(list, true)
	if err != nil {
		return "", err
	}
	task := map[string]any{"title": title, "notes": notes}
	if v := dueValue(sub(due, 0, 10)); v != "" {
		task["due"] = v
	}
	var answer map[string]any
	if err := a.tasks("POST", "/lists/"+enc(l.ID)+"/tasks", task, &answer); err != nil {
		return "", err
	}
	a.refreshAgenda()
	return l.ID + "/" + digS(answer, "id"), nil
}

// CompleteTodo ticks a to-do off ("<list>/<task>"); one deleted meanwhile counts as done.
func (a *Account) CompleteTodo(id string) error {
	listID, taskID, ok := splitTaskID(id)
	if !ok {
		return fmt.Errorf("bad task id %s", id)
	}
	err := a.tasks("PATCH", "/lists/"+enc(listID)+"/tasks/"+enc(taskID), map[string]any{"status": "completed"}, nil)
	if StatusOf(err) == 404 {
		return nil
	}
	if err == nil {
		a.refreshAgenda()
	}
	return err
}

// OpenTodos: the ids of the open to-dos of the list named list (nil, nil if there is none), so
// what the user ticked off or deleted in the app can be told apart.
func (a *Account) OpenTodos(list string) (map[string]bool, error) {
	l, err := a.findList(list, false)
	if err != nil {
		if strings.HasPrefix(err.Error(), "Error: no list") {
			return nil, nil
		}
		return nil, err
	}
	var answer map[string]any
	path := "/lists/" + enc(l.ID) + "/tasks?maxResults=" + strconv.Itoa(maxTasks) + "&showCompleted=false"
	if err := a.tasks("GET", path, nil, &answer); err != nil {
		return nil, err
	}
	open := map[string]bool{}
	for _, task := range digL(answer, "items") {
		open[l.ID+"/"+digS(task, "id")] = true
	}
	return open, nil
}

// AddTodoTools registers list_todos, add_todo and change_todo (the M5 adds them after the maps
// tools). The agenda refresh hook is Hooks.RefreshAgenda.
func (a *Account) AddTodoTools(r *tools.Registry) {
	if !a.Enabled() {
		return
	}
	r.Add(tools.Tool{
		Name: "list_todos",
		Description: "Reads the user's to-do lists in Google Tasks (the Tasks app on their phone): open tasks with due date " +
			"and id, per list. For 'was steht auf meiner Liste', 'was muss ich noch erledigen'.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"list": {"type": "string", "description": "list name, '' = all lists"},
                           "include_done": {"type": "boolean", "description": "also completed tasks"}},
            "required": ["list"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.listTodos})
	r.Add(tools.Tool{
		Name: "add_todo",
		Description: "Adds a task to one of the user's Google Tasks lists (a list that does not exist yet is made, e.g. " +
			"'Baumarkt'). Not for groceries: those go on the shopping list in Home Assistant. For a spoken reminder at " +
			"a time use create_task.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"title": {"type": "string", "description": "short, German"},
                           "list": {"type": "string", "description": "list name, '' = the default list"},
                           "notes": {"type": "string"},
                           "due": {"type": "string", "description": "YYYY-MM-DD, '' = none"}},
            "required": ["title", "list"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.addTodo})
	r.Add(tools.Tool{
		Name: "change_todo",
		Description: "Changes a task of the user's Google Tasks lists (id from list_todos): complete ('ist erledigt'), reopen, " +
			"edit title, notes or due date, or delete it.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"},
                           "action": {"type": "string", "enum": ["complete", "reopen", "edit", "delete"]},
                           "title": {"type": "string"},
                           "notes": {"type": "string"},
                           "due": {"type": "string", "description": "YYYY-MM-DD, '' removes it"}},
            "required": ["id", "action"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.changeTodo})
}
