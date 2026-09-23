package core

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// PlatformFactory creates a Platform from config options.
type PlatformFactory func(opts map[string]any) (Platform, error)

// AgentFactory creates an Agent from config options.
type AgentFactory func(opts map[string]any) (Agent, error)

// AgentControllerFactory creates a direct controller from agent options.
type AgentControllerFactory func(opts map[string]any) (AgentController, error)

var (
	platformFactories = make(map[string]PlatformFactory)
	agentFactories    = make(map[string]AgentFactory)

	agentControllerMu        sync.RWMutex
	agentControllerFactories = make(map[string]AgentControllerFactory)
)

func RegisterPlatform(name string, factory PlatformFactory) {
	platformFactories[name] = factory
}

func RegisterAgent(name string, factory AgentFactory) {
	agentFactories[name] = factory
}

// RegisterAgentController registers a direct controller at package
// initialization. Reusing a name is a programming error because it can
// redirect external-agent control to a different backend implementation.
func RegisterAgentController(name string, factory AgentControllerFactory) {
	if strings.TrimSpace(name) == "" {
		panic("register agent controller: empty name")
	}
	if factory == nil {
		panic("register agent controller: nil factory")
	}
	agentControllerMu.Lock()
	defer agentControllerMu.Unlock()
	if _, exists := agentControllerFactories[name]; exists {
		panic(fmt.Sprintf("register agent controller: duplicate name %q", name))
	}
	agentControllerFactories[name] = factory
}

func CreatePlatform(name string, opts map[string]any) (Platform, error) {
	f, ok := platformFactories[name]
	if !ok {
		available := make([]string, 0, len(platformFactories))
		for k := range platformFactories {
			available = append(available, k)
		}
		return nil, fmt.Errorf("unknown platform %q, available: %v", name, available)
	}
	return f(opts)
}

func ListRegisteredAgents() []string {
	names := make([]string, 0, len(agentFactories))
	for k := range agentFactories {
		names = append(names, k)
	}
	return names
}

func ListRegisteredPlatforms() []string {
	names := make([]string, 0, len(platformFactories))
	for k := range platformFactories {
		names = append(names, k)
	}
	return names
}

func ListRegisteredAgentControllers() []string {
	agentControllerMu.RLock()
	names := make([]string, 0, len(agentControllerFactories))
	for name := range agentControllerFactories {
		names = append(names, name)
	}
	agentControllerMu.RUnlock()
	sort.Strings(names)
	return names
}

func CreateAgent(name string, opts map[string]any) (Agent, error) {
	f, ok := agentFactories[name]
	if !ok {
		available := make([]string, 0, len(agentFactories))
		for k := range agentFactories {
			available = append(available, k)
		}
		return nil, fmt.Errorf("unknown agent %q, available: %v", name, available)
	}
	return f(opts)
}

// CreateAgentController creates a controller. Controller creation configures a
// transport only; implementations MUST NOT create or start an external target.
func CreateAgentController(name string, opts map[string]any) (AgentController, error) {
	agentControllerMu.RLock()
	factory, ok := agentControllerFactories[name]
	names := make([]string, 0, len(agentControllerFactories))
	if !ok {
		for registeredName := range agentControllerFactories {
			names = append(names, registeredName)
		}
	}
	agentControllerMu.RUnlock()
	if !ok {
		sort.Strings(names)
		return nil, fmt.Errorf("%w %q, available: %v", ErrAgentControllerNotRegistered, name, names)
	}
	return factory(opts)
}
