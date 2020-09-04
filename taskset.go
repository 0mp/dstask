package dstask

// main task data structures

import (
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type TaskSet struct {
	tasks []*Task

	// indices
	tasksByID   map[int]*Task
	tasksByUUID map[string]*Task

	// program metadata
	idsFilePath   string
	stateFilePath string
	repoPath      string
}

type Project struct {
	Name          string
	Tasks         int
	TasksResolved int
	// if any task is in the active state
	Active bool
	// first task created
	Created time.Time
	// last task resolved
	Resolved time.Time

	// highest non-resolved priority within project
	Priority string
}

// NewTaskSet constructs a TaskSet from a repo path and zero or more options.
func NewTaskSet(repoPath, idsFilePath, stateFilePath string, opts ...TaskSetOpt) (*TaskSet, error) {

	// Initialise an empty TaskSet
	var ts TaskSet
	ts.tasksByUUID = make(map[string]*Task)
	ts.tasksByID = make(map[int]*Task)

	ts.idsFilePath = idsFilePath
	ts.stateFilePath = stateFilePath
	ts.repoPath = repoPath

	// Apply our options
	var tso taskSetOpts
	for _, opt := range opts {
		opt(&tso)
	}
	ids := LoadIds(idsFilePath)

	filteredStatuses := filterStringSlice(tso.statuses, tso.withoutStatuses)

	for _, status := range filteredStatuses {
		dir := filepath.Join(repoPath, status)
		files, err := ioutil.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				// Continuing here is necessary, because we do not guarantee
				// that all status directories exist on program startup.
				continue
			}
			return nil, err
		}
		for _, finfo := range files {
			path := filepath.Join(dir, finfo.Name())
			t, err := unmarshalTask(path, finfo, ids, status)
			if err != nil {
				log.Printf("error loading task: %v\n", err)
				continue
			}
			ts.LoadTask(t)
		}
	}

	return &ts, nil
}

type TaskSetOpt func(opts *taskSetOpts)

func WithStatuses(statuses ...string) TaskSetOpt {
	return func(opts *taskSetOpts) {
		opts.statuses = append(opts.statuses, statuses...)
	}
}

func WithoutStatuses(statuses ...string) TaskSetOpt {
	return func(opts *taskSetOpts) {
		opts.withoutStatuses = append(opts.withoutStatuses, statuses...)
	}
}

type taskSetOpts struct {
	statuses        []string
	withoutStatuses []string
}

// todo add test
func filterStringSlice(with, without []string) []string {
	var ret []string
	for _, wanted := range with {
		keep := true
		for _, unwanted := range without {
			if wanted == unwanted {
				keep = false
			}
		}
		if keep {
			ret = append(ret, wanted)
		}
	}
	return ret
}

func (ts *TaskSet) SortByPriority() {
	sort.SliceStable(ts.tasks, func(i, j int) bool { return ts.tasks[i].Created.Before(ts.tasks[j].Created) })
	sort.SliceStable(ts.tasks, func(i, j int) bool { return ts.tasks[i].Priority < ts.tasks[j].Priority })
}

func (ts *TaskSet) SortByResolved() {
	sort.SliceStable(ts.tasks, func(i, j int) bool { return ts.tasks[i].Resolved.Before(ts.tasks[j].Resolved) })
}

// LoadTask adds a task to the TaskSet, but only if it has a new uuid or no uuid.
// Return annotated task.
func (ts *TaskSet) LoadTask(task Task) Task {
	task.Normalise()

	if task.UUID == "" {
		task.UUID = MustGetUUID4String()
	}

	if err := task.Validate(); err != nil {
		ExitFail("%s, task %s", err, task.UUID)
	}

	if ts.tasksByUUID[task.UUID] != nil {
		// load tasks, do not overwrite
		// TODO ??? (maybe return a nil pointer instead?)
		return Task{}
	}

	// remove ID if already taken
	if task.ID > 0 && ts.tasksByID[task.ID] != nil {
		task.ID = 0
	}

	// pick one if task isn't resolved and ID isn't there
	if task.ID == 0 && task.Status != STATUS_RESOLVED {
		for id := 1; id <= MAX_TASKS_OPEN; id++ {
			if ts.tasksByID[id] == nil {
				task.ID = id
				break
			}
		}
	}

	if task.Created.IsZero() {
		task.Created = time.Now()
		task.WritePending = true
	}

	ts.tasks = append(ts.tasks, &task)
	ts.tasksByUUID[task.UUID] = &task
	ts.tasksByID[task.ID] = &task

	return task
}

// TODO maybe this is the place to check for invalid state transitions instead
// of the main switch statement. Though, a future 3rdparty sync system could
// need this to work regardless.
func (ts *TaskSet) MustUpdateTask(task Task) {
	task.Normalise()

	if err := task.Validate(); err != nil {
		ExitFail("%s, task %s", err, task.UUID)
	}

	if ts.tasksByUUID[task.UUID] == nil {
		ExitFail("Could not find given task to update by UUID")
	}

	if !IsValidPriority(task.Priority) {
		ExitFail("Invalid priority specified")
	}

	old := ts.tasksByUUID[task.UUID]

	if old.Status != task.Status && !IsValidStateTransition(old.Status, task.Status) {
		ExitFail("Invalid state transition: %s -> %s", old.Status, task.Status)
	}

	if old.Status != task.Status && task.Status == STATUS_RESOLVED && strings.Contains(task.Notes, "- [ ] ") {
		ExitFail("Refusing to resolve task with incomplete tasklist")
	}

	if task.Status == STATUS_RESOLVED {
		task.ID = 0
	}

	if task.Status == STATUS_RESOLVED && task.Resolved.IsZero() {
		task.Resolved = time.Now()
	}

	task.WritePending = true
	// existing pointer must point to address of new task copied
	*ts.tasksByUUID[task.UUID] = task
}

func (ts *TaskSet) Filter(cmdLine CmdLine) {
	for _, task := range ts.tasks {
		if !task.MatchesFilter(cmdLine) {
			task.filtered = true
		}
	}
}

func (ts *TaskSet) FilterByStatus(status string) {
	for _, task := range ts.tasks {
		if task.Status != status {
			task.filtered = true
		}
	}
}

func (ts *TaskSet) FilterOutStatus(status string) {
	for _, task := range ts.tasks {
		if task.Status == status {
			task.filtered = true
		}
	}
}

func (ts *TaskSet) FilterUnorganised() {
	for _, task := range ts.tasks {
		if len(task.Tags) > 0 || task.Project != "" {
			task.filtered = true
		}
	}
}

func (ts *TaskSet) MustGetByID(id int) Task {
	if ts.tasksByID[id] == nil {
		ExitFail("No open task with ID %v exists.", id)
	}

	return *ts.tasksByID[id]
}

func (ts *TaskSet) Tasks() []Task {
	tasks := make([]Task, 0, len(ts.tasks))
	for _, task := range ts.tasks {
		if !task.filtered {
			tasks = append(tasks, *task)
		}
	}
	return tasks
}

func (ts *TaskSet) AllTasks() []Task {
	tasks := make([]Task, 0, len(ts.tasks))
	for _, task := range ts.tasks {
		tasks = append(tasks, *task)
	}
	return tasks
}

func (ts *TaskSet) GetTags() map[string]bool {
	tagset := make(map[string]bool)

	for _, task := range ts.Tasks() {
		for _, tag := range task.Tags {
			tagset[tag] = true
		}
	}

	return tagset
}

func (ts *TaskSet) GetProjects() map[string]*Project {
	projects := make(map[string]*Project)

	for _, task := range ts.Tasks() {
		name := task.Project

		if name == "" {
			continue
		}

		if projects[name] == nil {
			projects[name] = &Project{
				Name:     name,
				Priority: PRIORITY_LOW,
			}
		}

		project := projects[name]

		project.Tasks += 1

		if project.Created.IsZero() || task.Created.Before(project.Created) {
			project.Created = task.Created
		}

		if task.Resolved.After(project.Resolved) {
			project.Resolved = task.Resolved
		}

		if task.Status == STATUS_RESOLVED {
			project.TasksResolved += 1
		}

		if task.Status == STATUS_ACTIVE {
			project.Active = true
		}

		if task.Status != STATUS_RESOLVED && task.Priority < project.Priority {
			project.Priority = task.Priority
		}
	}

	return projects
}

func (ts *TaskSet) NumTotal() int {
	return len(ts.tasks)
}

// save pending changes to disk
// TODO return files that have been added/deleted/modified/renamed so they can
// be passed to git add for performance, instead of doing git add .
func (ts *TaskSet) SavePendingChanges() {
	ids := make(IdsMap, len(ts.Tasks()))

	for _, task := range ts.tasks {
		if task.WritePending {
			task.SaveToDisk(ts.repoPath)
		}

		if task.ID > 0 {
			ids[task.UUID] = task.ID
		}
	}

	// saving generally only happens when tasks are mutated. This is OK, and
	// important. Generally the ID assignment process is deterministic such
	// that a DB is not required. However, if tasks are listed and then tasks
	// are closed or created, it can have a ripple effect such that it is
	// possible for every ID to change. Therefore, tasks must retain their IDs
	// locally. This replaced a system where tasks recorded their IDs, which
	// can create merge conflicts in some (uncommon) cases.
	ids.Save(ts.idsFilePath)
}
