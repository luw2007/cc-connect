package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

const defaultAgentTailLines = 100

var errAgentUsage = errors.New("show agent usage")

type agentCommand struct {
	action          string
	backend         string
	project         string
	configPath      string
	socketPath      string
	id              string
	revision        string
	requestID       string
	requestRevision string
	message         string
	key             string
	mode            string
	lines           int
	useStdin        bool
	json            bool
	answers         []core.AgentControlQuestionAnswer
}

type agentCommandDeps struct {
	createController  func(string, map[string]any) (core.AgentController, error)
	loadConfig        func(string) (*config.Config, error)
	resolveConfigPath func(string) string
}

func defaultAgentCommandDeps() agentCommandDeps {
	return agentCommandDeps{
		createController:  core.CreateAgentController,
		loadConfig:        config.LoadPermissive,
		resolveConfigPath: resolveConfigPath,
	}
}

func runAgent(args []string) {
	err := executeAgent(context.Background(), args, os.Stdin, os.Stdout, defaultAgentCommandDeps())
	if errors.Is(err, errAgentUsage) {
		printAgentUsage(os.Stdout)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func dispatchAgentSubcommand(args []string, runner func([]string)) bool {
	if len(args) == 0 || args[0] != "agent" {
		return false
	}
	runner(args[1:])
	return true
}

func executeAgent(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, deps agentCommandDeps) error {
	command, err := parseAgentArgs(args)
	if err != nil {
		return err
	}
	controller, err := resolveAgentController(command, deps)
	if err != nil {
		return err
	}

	targets, err := controller.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	if command.action == "list" {
		return outputAgentTargets(stdout, targets, command.json)
	}

	targetRef, err := agentTargetRef(command.id, command.revision)
	if err != nil {
		return err
	}
	target, err := exactAgentTarget(targets, targetRef)
	if err != nil {
		return err
	}

	switch command.action {
	case "tail":
		tailer, err := agentTailer(controller, target)
		if err != nil {
			return err
		}
		text, err := tailer.TailAgent(ctx, targetRef, command.lines)
		if err != nil {
			return fmt.Errorf("tail agent %q: %w", targetRef.ID, err)
		}
		return outputAgentTail(stdout, text, command.json)
	case "send":
		prompter, err := agentPrompter(controller, target)
		if err != nil {
			return err
		}
		message := command.message
		if command.useStdin {
			data, readErr := io.ReadAll(stdin)
			if readErr != nil {
				return fmt.Errorf("read prompt from stdin: %w", readErr)
			}
			message = string(data)
			if strings.TrimSpace(message) == "" {
				return fmt.Errorf("--stdin produced an empty message")
			}
			message = strings.TrimRight(message, "\r\n")
		}
		if err := prompter.SendAgentPrompt(ctx, targetRef, message); err != nil {
			return fmt.Errorf("send prompt to agent %q: %w", targetRef.ID, err)
		}
		return outputAgentMutation(stdout, command.json, "Prompt sent.")
	case "key":
		sender, err := agentKeySender(controller, target)
		if err != nil {
			return err
		}
		if err := sender.SendAgentKey(ctx, targetRef, command.key); err != nil {
			return fmt.Errorf("send key to agent %q: %w", targetRef.ID, err)
		}
		return outputAgentMutation(stdout, command.json, "Key sent.")
	case "requests":
		lister, err := agentRequestLister(controller, target)
		if err != nil {
			return err
		}
		requests, err := lister.ListAgentRequests(ctx, targetRef)
		if err != nil {
			return fmt.Errorf("list requests for agent %q: %w", targetRef.ID, err)
		}
		return outputAgentRequests(stdout, requests, command.json)
	case "approve":
		lister, responder, err := agentPermissionCapabilities(controller, target)
		if err != nil {
			return err
		}
		requestRef, err := agentRequestRef(command.requestID, command.requestRevision)
		if err != nil {
			return err
		}
		request, err := inspectExactAgentRequest(ctx, lister, targetRef, requestRef)
		if err != nil {
			return err
		}
		if err := responder.RespondAgentPermission(ctx, targetRef, request.Ref(), command.mode); err != nil {
			return fmt.Errorf("respond to permission %q: %w", requestRef.ID, err)
		}
		return outputAgentMutation(stdout, command.json, "Permission response sent.")
	case "answer":
		lister, responder, err := agentQuestionCapabilities(controller, target)
		if err != nil {
			return err
		}
		requestRef, err := agentRequestRef(command.requestID, command.requestRevision)
		if err != nil {
			return err
		}
		request, err := inspectExactAgentRequest(ctx, lister, targetRef, requestRef)
		if err != nil {
			return err
		}
		if err := responder.AnswerAgentQuestions(ctx, targetRef, request.Ref(), command.answers); err != nil {
			return fmt.Errorf("answer question %q: %w", requestRef.ID, err)
		}
		return outputAgentMutation(stdout, command.json, "Question answer sent.")
	default:
		return fmt.Errorf("unknown agent action %q", command.action)
	}
}

func parseAgentArgs(args []string) (agentCommand, error) {
	command := agentCommand{lines: defaultAgentTailLines}
	if len(args) == 0 {
		return command, errAgentUsage
	}
	command.action = args[0]
	if command.action == "help" || command.action == "--help" || command.action == "-h" {
		return command, errAgentUsage
	}
	switch command.action {
	case "list", "tail", "send", "key", "requests", "approve", "answer":
	default:
		return command, fmt.Errorf("unknown agent command %q", command.action)
	}

	seen := make(map[string]bool)
	selectionAnswers := make(map[int][]string)
	textAnswers := make(map[int]string)
	var choice string
	choiceSet := false

	for i := 1; i < len(args); i++ {
		name, inlineValue, inline := splitAgentFlag(args[i])
		if name == "--help" || name == "-h" {
			return command, errAgentUsage
		}
		if name == "--json" || name == "--stdin" {
			if inline {
				return command, fmt.Errorf("%s does not take a value", name)
			}
			if seen[name] {
				return command, fmt.Errorf("%s may be specified only once", name)
			}
			seen[name] = true
			if name == "--json" {
				command.json = true
			} else {
				command.useStdin = true
			}
			continue
		}

		value, err := agentFlagValue(args, &i, name, inlineValue, inline)
		if err != nil {
			return command, err
		}
		repeatable := name == "--answer" || name == "--text"
		if seen[name] && !repeatable {
			return command, fmt.Errorf("%s may be specified only once", name)
		}
		seen[name] = true

		switch name {
		case "--backend":
			command.backend = value
		case "--project":
			command.project = value
		case "--config":
			command.configPath = value
		case "--socket":
			command.socketPath = value
		case "--id":
			command.id = value
		case "--revision":
			command.revision = value
		case "--request":
			command.requestID = value
		case "--request-revision":
			command.requestRevision = value
		case "--message":
			command.message = value
		case "--key":
			command.key = value
		case "--mode":
			command.mode = value
		case "--lines":
			lines, parseErr := strconv.Atoi(value)
			if parseErr != nil || lines <= 0 {
				return command, fmt.Errorf("--lines must be a positive integer")
			}
			command.lines = lines
		case "--choice":
			choice = value
			choiceSet = true
		case "--answer":
			index, answer, parseErr := parseIndexedAgentAnswer(name, value)
			if parseErr != nil {
				return command, parseErr
			}
			selectionAnswers[index] = append(selectionAnswers[index], answer)
		case "--text":
			index, answer, parseErr := parseIndexedAgentAnswer(name, value)
			if parseErr != nil {
				return command, parseErr
			}
			if _, exists := textAnswers[index]; exists {
				return command, fmt.Errorf("--text index %d may be specified only once", index)
			}
			textAnswers[index] = answer
		default:
			return command, fmt.Errorf("unknown option %q", name)
		}
	}

	for _, source := range []struct {
		name  string
		value string
	}{
		{"--backend", command.backend},
		{"--project", command.project},
		{"--config", command.configPath},
		{"--socket", command.socketPath},
	} {
		if seen[source.name] && strings.TrimSpace(source.value) == "" {
			return command, fmt.Errorf("%s requires a nonblank value", source.name)
		}
	}
	if command.backend != "" && command.backend != "cmux" && command.backend != "herdr" {
		return command, fmt.Errorf("--backend must be cmux or herdr")
	}
	if choiceSet {
		if strings.TrimSpace(choice) == "" {
			return command, fmt.Errorf("--choice requires a nonblank value")
		}
		if len(selectionAnswers[0]) != 0 || textAnswers[0] != "" {
			return command, fmt.Errorf("--choice conflicts with another answer for question index 0")
		}
		selectionAnswers[0] = []string{choice}
	}
	answers, err := mergeIndexedAgentAnswers(selectionAnswers, textAnswers)
	if err != nil {
		return command, err
	}
	command.answers = answers

	if err := validateAgentCommand(command, seen); err != nil {
		return command, err
	}
	return command, nil
}

func splitAgentFlag(argument string) (name, value string, inline bool) {
	if strings.HasPrefix(argument, "--") {
		if index := strings.IndexByte(argument, '='); index >= 0 {
			return argument[:index], argument[index+1:], true
		}
	}
	return argument, "", false
}

func agentFlagValue(args []string, index *int, name, inlineValue string, inline bool) (string, error) {
	if !strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("unexpected argument %q", name)
	}
	if inline {
		return inlineValue, nil
	}
	if *index+1 >= len(args) {
		return "", fmt.Errorf("%s requires a value", name)
	}
	*index++
	return args[*index], nil
}

func parseIndexedAgentAnswer(flag, value string) (int, string, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("%s requires INDEX:VALUE", flag)
	}
	index, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || index < 0 {
		return 0, "", fmt.Errorf("%s requires a non-negative numeric index", flag)
	}
	answer := parts[1]
	if strings.TrimSpace(answer) == "" {
		return 0, "", fmt.Errorf("%s requires a nonblank value", flag)
	}
	return index, answer, nil
}

func mergeIndexedAgentAnswers(selections map[int][]string, texts map[int]string) ([]core.AgentControlQuestionAnswer, error) {
	indices := make([]int, 0, len(selections)+len(texts))
	for index := range selections {
		if _, conflict := texts[index]; conflict {
			return nil, fmt.Errorf("question index %d cannot have both selections and text", index)
		}
		indices = append(indices, index)
	}
	for index := range texts {
		indices = append(indices, index)
	}
	sort.Ints(indices)

	answers := make([]core.AgentControlQuestionAnswer, 0, len(indices))
	for _, index := range indices {
		answer := core.AgentControlQuestionAnswer{Index: index}
		if values, ok := selections[index]; ok {
			answer.Selections = values
		} else {
			answer.Text = texts[index]
		}
		if !answer.Valid() {
			return nil, fmt.Errorf("%w: invalid answer for question index %d", core.ErrAgentControlInvalidAnswer, index)
		}
		answers = append(answers, answer)
	}
	return answers, nil
}

func validateAgentCommand(command agentCommand, seen map[string]bool) error {
	common := map[string]bool{
		"--backend": true, "--project": true, "--config": true, "--socket": true, "--json": true,
	}
	allowed := make(map[string]bool, len(common)+8)
	for name := range common {
		allowed[name] = true
	}
	if command.action != "list" {
		allowed["--id"] = true
		allowed["--revision"] = true
		if _, err := agentTargetRef(command.id, command.revision); err != nil {
			return err
		}
	}

	switch command.action {
	case "list":
	case "tail":
		allowed["--lines"] = true
	case "send":
		allowed["--message"] = true
		allowed["--stdin"] = true
		if seen["--message"] == command.useStdin {
			return fmt.Errorf("send requires exactly one of --message or --stdin")
		}
		if seen["--message"] && strings.TrimSpace(command.message) == "" {
			return fmt.Errorf("--message requires a nonblank value")
		}
	case "key":
		allowed["--key"] = true
		if !seen["--key"] || strings.TrimSpace(command.key) == "" {
			return fmt.Errorf("key requires --key with a nonblank value")
		}
	case "requests":
	case "approve":
		allowed["--request"] = true
		allowed["--request-revision"] = true
		allowed["--mode"] = true
		if _, err := agentRequestRef(command.requestID, command.requestRevision); err != nil {
			return err
		}
		if !seen["--mode"] || strings.TrimSpace(command.mode) == "" {
			return fmt.Errorf("approve requires --mode with a nonblank value")
		}
	case "answer":
		allowed["--request"] = true
		allowed["--request-revision"] = true
		allowed["--choice"] = true
		allowed["--answer"] = true
		allowed["--text"] = true
		if _, err := agentRequestRef(command.requestID, command.requestRevision); err != nil {
			return err
		}
		if len(command.answers) == 0 {
			return fmt.Errorf("%w: answer requires --choice, --answer, or --text", core.ErrAgentControlInvalidAnswer)
		}
	}

	for name := range seen {
		if !allowed[name] {
			return fmt.Errorf("%s is not valid for agent %s", name, command.action)
		}
	}
	return nil
}

func agentTargetRef(id, revision string) (core.AgentControlTargetRef, error) {
	ref := core.AgentControlTargetRef{ID: id, Revision: revision}
	if err := core.ValidateAgentControlTargetRef(ref); err != nil {
		return core.AgentControlTargetRef{}, fmt.Errorf("%w: --id and --revision are required", err)
	}
	return ref, nil
}

func agentRequestRef(id, revision string) (core.AgentControlRequestRef, error) {
	ref := core.AgentControlRequestRef{ID: id, Revision: revision}
	if err := core.ValidateAgentControlRequestRef(ref); err != nil {
		return core.AgentControlRequestRef{}, fmt.Errorf("%w: --request and --request-revision are required", err)
	}
	return ref, nil
}

func resolveAgentController(command agentCommand, deps agentCommandDeps) (core.AgentController, error) {
	backend := command.backend
	opts := make(map[string]any)
	usesConfig := command.project != "" || command.configPath != "" || backend == ""
	if usesConfig {
		path := deps.resolveConfigPath(command.configPath)
		cfg, err := deps.loadConfig(path)
		if err != nil {
			return nil, fmt.Errorf("load config %s: %w", path, err)
		}
		project, err := selectAgentProject(cfg.Projects, command.project)
		if err != nil {
			return nil, err
		}
		if backend != "" && backend != project.Agent.Type {
			return nil, fmt.Errorf("backend %q does not match project %q agent type %q", backend, project.Name, project.Agent.Type)
		}
		backend = project.Agent.Type
		for name, value := range project.Agent.Options {
			opts[name] = value
		}
	}
	if command.socketPath != "" {
		opts["socket_path"] = command.socketPath
	}
	controller, err := deps.createController(backend, opts)
	if err != nil {
		return nil, fmt.Errorf("create %s agent controller: %w", backend, err)
	}
	return controller, nil
}

func selectAgentProject(projects []config.ProjectConfig, name string) (config.ProjectConfig, error) {
	if name == "" {
		if len(projects) != 1 {
			return config.ProjectConfig{}, fmt.Errorf("config has %d projects; specify exactly one with --project", len(projects))
		}
		return projects[0], nil
	}
	var selected *config.ProjectConfig
	for index := range projects {
		if projects[index].Name != name {
			continue
		}
		if selected != nil {
			return config.ProjectConfig{}, fmt.Errorf("project %q is ambiguous in config", name)
		}
		selected = &projects[index]
	}
	if selected == nil {
		return config.ProjectConfig{}, fmt.Errorf("project %q not found in config", name)
	}
	return *selected, nil
}

func exactAgentTarget(targets []core.AgentControlTarget, ref core.AgentControlTargetRef) (core.AgentControlTarget, error) {
	var matched *core.AgentControlTarget
	for index := range targets {
		if targets[index].ID != ref.ID {
			continue
		}
		if matched != nil {
			return core.AgentControlTarget{}, fmt.Errorf("%w: duplicate target identity %q", core.ErrAgentControlTargetStale, ref.ID)
		}
		matched = &targets[index]
	}
	if matched == nil || matched.Revision != ref.Revision {
		return core.AgentControlTarget{}, fmt.Errorf("%w: %q", core.ErrAgentControlTargetStale, ref.ID)
	}
	return *matched, nil
}

func inspectExactAgentRequest(ctx context.Context, lister core.AgentControlRequestLister, target core.AgentControlTargetRef, ref core.AgentControlRequestRef) (core.AgentControlRequest, error) {
	requests, err := lister.ListAgentRequests(ctx, target)
	if err != nil {
		return core.AgentControlRequest{}, fmt.Errorf("list requests for agent %q: %w", target.ID, err)
	}
	var matched *core.AgentControlRequest
	for index := range requests {
		if requests[index].ID != ref.ID {
			continue
		}
		if matched != nil {
			return core.AgentControlRequest{}, fmt.Errorf("%w: duplicate request identity %q", core.ErrAgentControlRequestStale, ref.ID)
		}
		matched = &requests[index]
	}
	if matched == nil || matched.Revision != ref.Revision {
		return core.AgentControlRequest{}, fmt.Errorf("%w: %q", core.ErrAgentControlRequestStale, ref.ID)
	}
	return *matched, nil
}

func agentTailer(controller core.AgentController, target core.AgentControlTarget) (core.AgentControlTailer, error) {
	if !target.Supports(core.AgentControlCapabilityTail) || !core.AgentControlCapabilitySupported(controller, core.AgentControlCapabilityTail) {
		return nil, unsupportedAgentCapability(target.ID, core.AgentControlCapabilityTail)
	}
	capability, _ := controller.(core.AgentControlTailer)
	return capability, nil
}

func agentPrompter(controller core.AgentController, target core.AgentControlTarget) (core.AgentControlPrompter, error) {
	if !target.Supports(core.AgentControlCapabilityPrompt) || !core.AgentControlCapabilitySupported(controller, core.AgentControlCapabilityPrompt) {
		return nil, unsupportedAgentCapability(target.ID, core.AgentControlCapabilityPrompt)
	}
	capability, _ := controller.(core.AgentControlPrompter)
	return capability, nil
}

func agentKeySender(controller core.AgentController, target core.AgentControlTarget) (core.AgentControlKeySender, error) {
	if !target.Supports(core.AgentControlCapabilityKey) || !core.AgentControlCapabilitySupported(controller, core.AgentControlCapabilityKey) {
		return nil, unsupportedAgentCapability(target.ID, core.AgentControlCapabilityKey)
	}
	capability, _ := controller.(core.AgentControlKeySender)
	return capability, nil
}

func agentRequestLister(controller core.AgentController, target core.AgentControlTarget) (core.AgentControlRequestLister, error) {
	if !target.Supports(core.AgentControlCapabilityRequests) || !core.AgentControlCapabilitySupported(controller, core.AgentControlCapabilityRequests) {
		return nil, unsupportedAgentCapability(target.ID, core.AgentControlCapabilityRequests)
	}
	capability, _ := controller.(core.AgentControlRequestLister)
	return capability, nil
}

func agentPermissionCapabilities(controller core.AgentController, target core.AgentControlTarget) (core.AgentControlRequestLister, core.AgentControlPermissionResponder, error) {
	lister, err := agentRequestLister(controller, target)
	if err != nil {
		return nil, nil, err
	}
	if !target.Supports(core.AgentControlCapabilityPermission) || !core.AgentControlCapabilitySupported(controller, core.AgentControlCapabilityPermission) {
		return nil, nil, unsupportedAgentCapability(target.ID, core.AgentControlCapabilityPermission)
	}
	responder, _ := controller.(core.AgentControlPermissionResponder)
	return lister, responder, nil
}

func agentQuestionCapabilities(controller core.AgentController, target core.AgentControlTarget) (core.AgentControlRequestLister, core.AgentControlQuestionResponder, error) {
	lister, err := agentRequestLister(controller, target)
	if err != nil {
		return nil, nil, err
	}
	if !target.Supports(core.AgentControlCapabilityQuestion) || !core.AgentControlCapabilitySupported(controller, core.AgentControlCapabilityQuestion) {
		return nil, nil, unsupportedAgentCapability(target.ID, core.AgentControlCapabilityQuestion)
	}
	responder, _ := controller.(core.AgentControlQuestionResponder)
	return lister, responder, nil
}

func unsupportedAgentCapability(id string, capability core.AgentControlCapability) error {
	return fmt.Errorf("%w: target %q does not support %s", core.ErrAgentControlUnsupported, id, capability)
}

func outputAgentTargets(writer io.Writer, targets []core.AgentControlTarget, jsonOutput bool) error {
	if jsonOutput {
		return writeAgentJSON(writer, targets)
	}
	if len(targets) == 0 {
		fmt.Fprintln(writer, "(no agent targets)")
	} else {
		table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "ID\tREVISION\tBACKEND\tKIND\tSTATUS\tCAPABILITIES\tDIRECTORY")
		for _, target := range targets {
			capabilities := make([]string, len(target.Capabilities))
			for index, capability := range target.Capabilities {
				capabilities[index] = string(capability)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", target.ID, abbreviateAgentRevision(target.Revision), target.Backend, target.Kind, target.Status, strings.Join(capabilities, ","), target.Directory)
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintln(writer, "Use --json to copy the full --id and --revision values for actions.")
	return nil
}

func outputAgentRequests(writer io.Writer, requests []core.AgentControlRequest, jsonOutput bool) error {
	if jsonOutput {
		return writeAgentJSON(writer, requests)
	}
	if len(requests) == 0 {
		fmt.Fprintln(writer, "(no pending requests)")
	} else {
		table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "ID\tREVISION\tKIND\tTOOL\tDECISIONS\tQUESTIONS")
		for _, request := range requests {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d\n", request.ID, abbreviateAgentRevision(request.Revision), request.Kind, request.ToolName, strings.Join(request.AllowedDecisions, ","), len(request.Questions))
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintln(writer, "Use --json to copy the full --request and --request-revision values for a response.")
	return nil
}

func outputAgentTail(writer io.Writer, text string, jsonOutput bool) error {
	if jsonOutput {
		return writeAgentJSON(writer, text)
	}
	if _, err := io.WriteString(writer, text); err != nil {
		return err
	}
	if !strings.HasSuffix(text, "\n") {
		_, err := io.WriteString(writer, "\n")
		return err
	}
	return nil
}

func outputAgentMutation(writer io.Writer, jsonOutput bool, message string) error {
	if jsonOutput {
		return writeAgentJSON(writer, nil)
	}
	_, err := fmt.Fprintln(writer, message)
	return err
}

func writeAgentJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func abbreviateAgentRevision(revision string) string {
	const visible = 12
	if len(revision) <= visible {
		return revision
	}
	return revision[:visible] + "…"
}

func printAgentUsage(writer io.Writer) {
	fmt.Fprint(writer, `Usage: cc-connect agent <command> [options]

Control existing agent targets without creating workspaces, panes, or agents.
Use --backend cmux|herdr directly, or select an exact project from config.

Commands:
  list      List existing targets and their current capabilities
  tail      Read recent target output
  key       Send one exact key to an existing target
  requests  List pending requests for an existing target
  approve   Respond to a permission request
  answer    Answer one or more indexed questions

Selection options:
  --backend cmux|herdr  Direct backend, or assertion for a selected project
  --project NAME        Exact project name from config
  --config PATH         Config path; without --project it must contain one project
  --socket PATH         Override socket_path without printing other options
  --json                Emit the bare core JSON value

Freshness options:
  --id ID                       Exact target ID from 'agent list --json'
  --revision REVISION           Exact target revision from 'agent list --json'
  --request REQUEST             Exact request ID from 'agent requests --json'
  --request-revision REVISION   Exact request revision from 'agent requests --json'

Command forms:
  cc-connect agent list [selection] [--json]
  cc-connect agent tail [selection] --id ID --revision REV [--lines 100] [--json]
  cc-connect agent key [selection] --id ID --revision REV --key KEY [--json]
  cc-connect agent requests [selection] --id ID --revision REV [--json]
  cc-connect agent approve [selection] --id ID --revision REV --request ID --request-revision REV --mode MODE [--json]
  cc-connect agent answer [selection] --id ID --revision REV --request ID --request-revision REV ANSWERS [--json]

Answer forms:
  --choice VALUE         Shorthand for one selection at question index 0; may combine with nonzero indexes
  --answer INDEX:VALUE   Indexed selection; repeat for more questions/selections

Safe flow:
  cc-connect agent list --backend herdr --json
  cc-connect agent tail --backend herdr --id TARGET --revision TARGET_REV
  cc-connect agent requests --backend herdr --id TARGET --revision TARGET_REV --json

For cmux mutations, first inspect a request with requests --json, then use
approve or answer with the exact target and request revisions.
`)
}
