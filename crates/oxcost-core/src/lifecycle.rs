//! Plugin lifecycle state machine (contract §6): pure state transitions,
//! no I/O. Illegal transitions return typed errors rather than panicking.

/// Lifecycle state of one registered plugin.
///
/// ```text
/// Registered → Probing → Serving ⇄ CircuitOpen → HalfOpen → Serving
///      any ────→ Disabled (manual switch, never auto-recovers)
///      any ────→ Removed (terminal)
/// ```
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum PluginState {
    /// Manifest validated and registered; probing not yet started.
    Registered,
    /// Health probe in flight (M1 models state only; probing is M1.5).
    Probing,
    /// Healthy and routable.
    Serving,
    /// Circuit open: failures hit the threshold, cooling down.
    CircuitOpen,
    /// Post-cooldown trial; success re-enters Serving, failure re-opens.
    HalfOpen,
    /// Manually disabled (`routing.enabled = false`); excluded from
    /// routing, catalog, and probing until re-enabled.
    Disabled,
    /// Deregistered; terminal.
    Removed,
}

/// Why a transition was refused.
#[derive(Debug, thiserror::Error, PartialEq, Eq, Clone, Copy)]
pub enum InvalidTransition {
    /// Terminal state: nothing may leave `Removed`.
    #[error("plugin is Removed; terminal state")]
    AlreadyRemoved,
    /// `Disabled` plugins do not probe, serve, or trip breakers.
    #[error("plugin is Disabled; enable it first")]
    DisabledIsFrozen,
    /// The source state has no edge to the requested target.
    #[error("illegal transition {from} → {to}")]
    Illegal {
        from: &'static str,
        to: &'static str,
    },
}

/// Lifecycle event emitted by a successful transition.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TransitionEvent {
    /// Plugin the transition applies to.
    pub plugin_id: String,
    /// State before the transition.
    pub from: PluginState,
    /// State after the transition.
    pub to: PluginState,
}

impl PluginState {
    /// Target state name (for error messages / logging).
    pub const fn name(self) -> &'static str {
        match self {
            PluginState::Registered => "Registered",
            PluginState::Probing => "Probing",
            PluginState::Serving => "Serving",
            PluginState::CircuitOpen => "CircuitOpen",
            PluginState::HalfOpen => "HalfOpen",
            PluginState::Disabled => "Disabled",
            PluginState::Removed => "Removed",
        }
    }

    /// Is this state routable (visible in `/v1/models` and routing)?
    pub const fn is_routable(self) -> bool {
        matches!(self, PluginState::Serving | PluginState::HalfOpen)
    }

    /// Start probing a freshly (re-)registered plugin.
    pub fn start_probing(self) -> Result<PluginState, InvalidTransition> {
        use InvalidTransition::*;
        use PluginState::*;
        match self {
            Registered | Disabled => Ok(Probing),
            Removed => Err(AlreadyRemoved),
            other => Err(Illegal {
                from: other.name(),
                to: "Probing",
            }),
        }
    }

    /// Probe succeeded: begin serving.
    pub fn probe_succeeded(self) -> Result<PluginState, InvalidTransition> {
        use InvalidTransition::*;
        use PluginState::*;
        match self {
            Probing | HalfOpen => Ok(Serving),
            Removed => Err(AlreadyRemoved),
            Disabled => Err(DisabledIsFrozen),
            other => Err(Illegal {
                from: other.name(),
                to: "Serving",
            }),
        }
    }

    /// Probe failed while Probing: stay (probing continues in M1.5).
    /// Probe failure while HalfOpen: circuit re-opens.
    pub fn probe_failed(self) -> Result<PluginState, InvalidTransition> {
        use InvalidTransition::*;
        use PluginState::*;
        match self {
            Probing => Ok(Probing),
            HalfOpen => Ok(CircuitOpen),
            Removed => Err(AlreadyRemoved),
            Disabled => Err(DisabledIsFrozen),
            other => Err(Illegal {
                from: other.name(),
                to: "CircuitOpen",
            }),
        }
    }

    /// Serving plugin failed enough to trip the breaker.
    pub fn trip_breaker(self) -> Result<PluginState, InvalidTransition> {
        use InvalidTransition::*;
        use PluginState::*;
        match self {
            Serving => Ok(CircuitOpen),
            Removed => Err(AlreadyRemoved),
            Disabled => Err(DisabledIsFrozen),
            other => Err(Illegal {
                from: other.name(),
                to: "CircuitOpen",
            }),
        }
    }

    /// Cooldown elapsed: half-open trial.
    pub fn cooldown_elapsed(self) -> Result<PluginState, InvalidTransition> {
        use InvalidTransition::*;
        use PluginState::*;
        match self {
            CircuitOpen => Ok(HalfOpen),
            Removed => Err(AlreadyRemoved),
            other => Err(Illegal {
                from: other.name(),
                to: "HalfOpen",
            }),
        }
    }

    /// Manual disable (`routing.enabled = false`). Legal from any live
    /// state — the switch is orthogonal to the circuit breaker (§6).
    pub fn disable(self) -> Result<PluginState, InvalidTransition> {
        use PluginState::*;
        match self {
            Disabled => Ok(Disabled), // idempotent
            Removed => Err(InvalidTransition::AlreadyRemoved),
            _ => Ok(Disabled),
        }
    }

    /// Manual enable: re-enter the register→probe flow (§6). The plugin
    /// never resumes its pre-disable state.
    pub fn enable(self) -> Result<PluginState, InvalidTransition> {
        use InvalidTransition::*;
        use PluginState::*;
        match self {
            Disabled => Ok(Probing),
            Removed => Err(AlreadyRemoved),
            other => Err(Illegal {
                from: other.name(),
                to: "Probing",
            }),
        }
    }

    /// Deregistration (contract §6: removal is immediate and terminal).
    pub fn remove(self) -> Result<PluginState, InvalidTransition> {
        match self {
            PluginState::Removed => Err(InvalidTransition::AlreadyRemoved),
            _ => Ok(PluginState::Removed),
        }
    }
}

/// A plugin record: manifest + current lifecycle state.
#[derive(Debug, Clone)]
pub struct PluginEntry {
    pub manifest: oxcost_contract::PluginManifest,
    pub state: PluginState,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn happy_path_reaches_serving_and_recovers() {
        let mut s = PluginState::Registered;
        s = s.start_probing().unwrap();
        assert_eq!(s, PluginState::Probing);
        s = s.probe_succeeded().unwrap();
        assert_eq!(s, PluginState::Serving);
        assert!(s.is_routable());

        // Trip → cool → half-open → back to serving.
        s = s.trip_breaker().unwrap();
        assert_eq!(s, PluginState::CircuitOpen);
        assert!(!s.is_routable());
        s = s.cooldown_elapsed().unwrap();
        assert_eq!(s, PluginState::HalfOpen);
        s = s.probe_succeeded().unwrap();
        assert_eq!(s, PluginState::Serving);
    }

    #[test]
    fn half_open_failure_reopens_circuit() {
        let mut s = PluginState::Serving.trip_breaker().unwrap();
        s = s.cooldown_elapsed().unwrap();
        s = s.probe_failed().unwrap();
        assert_eq!(s, PluginState::CircuitOpen);
    }

    #[test]
    fn probing_failure_stays_probing() {
        let s = PluginState::Registered.start_probing().unwrap();
        assert_eq!(s.probe_failed().unwrap(), PluginState::Probing);
    }

    #[test]
    fn disabled_is_orthogonal_to_breaker() {
        // Disable while CircuitOpen is legal.
        let s = PluginState::Serving.trip_breaker().unwrap();
        let s = s.disable().unwrap();
        assert_eq!(s, PluginState::Disabled);

        // Enable re-enters Probing, not the remembered CircuitOpen.
        let s = s.enable().unwrap();
        assert_eq!(s, PluginState::Probing);
    }

    #[test]
    fn disabled_is_frozen() {
        let s = PluginState::Disabled;
        assert_eq!(s.trip_breaker(), Err(InvalidTransition::DisabledIsFrozen));
        assert_eq!(
            s.probe_succeeded(),
            Err(InvalidTransition::DisabledIsFrozen)
        );
        assert_eq!(s.probe_failed(), Err(InvalidTransition::DisabledIsFrozen));
        // Disable is idempotent; enable works.
        assert_eq!(s.disable().unwrap(), PluginState::Disabled);
        assert_eq!(s.enable().unwrap(), PluginState::Probing);
    }

    #[test]
    fn removed_is_terminal() {
        for from in [
            PluginState::Registered,
            PluginState::Probing,
            PluginState::Serving,
            PluginState::CircuitOpen,
            PluginState::HalfOpen,
            PluginState::Disabled,
        ] {
            assert_eq!(from.remove().unwrap(), PluginState::Removed);
        }
        let s = PluginState::Removed;
        assert_eq!(s.remove(), Err(InvalidTransition::AlreadyRemoved));
        assert_eq!(s.start_probing(), Err(InvalidTransition::AlreadyRemoved));
        assert_eq!(s.enable(), Err(InvalidTransition::AlreadyRemoved));
        assert_eq!(s.disable(), Err(InvalidTransition::AlreadyRemoved));
    }

    #[test]
    fn illegal_transitions_rejected() {
        // Serving cannot re-probe or skip to half-open.
        assert!(PluginState::Serving.start_probing().is_err());
        assert!(PluginState::Serving.cooldown_elapsed().is_err());
        // Registered cannot serve directly.
        assert!(PluginState::Registered.probe_succeeded().is_err());
        // CircuitOpen cannot serve directly.
        assert!(PluginState::CircuitOpen.probe_succeeded().is_err());
        // Only Serving can trip.
        assert!(PluginState::Probing.trip_breaker().is_err());
        // Only Disabled can enable.
        assert!(PluginState::Serving.enable().is_err());
    }

    #[test]
    fn enable_from_registered_is_illegal_but_start_probing_works() {
        assert!(PluginState::Registered.enable().is_err());
        assert_eq!(
            PluginState::Registered.start_probing().unwrap(),
            PluginState::Probing
        );
    }
}
