//! Capability model (contract §4): 13 capability groups, core vs
//! experimental tiers, and the `batches → files` dependency.

/// A capability group exposed by a plugin.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum Capability {
    Chat,
    Embeddings,
    Images,
    AudioSpeech,
    AudioTranscription,
    AudioTranslations,
    Video,
    Moderations,
    Responses,
    WebSearch,
    Files,
    Batches,
    Realtime,
}

impl Capability {
    /// All capability groups in declaration order.
    pub const ALL: [Capability; 13] = [
        Capability::Chat,
        Capability::Embeddings,
        Capability::Images,
        Capability::AudioSpeech,
        Capability::AudioTranscription,
        Capability::AudioTranslations,
        Capability::Video,
        Capability::Moderations,
        Capability::Responses,
        Capability::WebSearch,
        Capability::Files,
        Capability::Batches,
        Capability::Realtime,
    ];

    /// Stable kebab-case name (used in logs and the registration table).
    pub const fn as_str(self) -> &'static str {
        match self {
            Capability::Chat => "chat",
            Capability::Embeddings => "embeddings",
            Capability::Images => "images",
            Capability::AudioSpeech => "audio_speech",
            Capability::AudioTranscription => "audio_transcription",
            Capability::AudioTranslations => "audio_translations",
            Capability::Video => "video",
            Capability::Moderations => "moderations",
            Capability::Responses => "responses",
            Capability::WebSearch => "web_search",
            Capability::Files => "files",
            Capability::Batches => "batches",
            Capability::Realtime => "realtime",
        }
    }

    /// Contract tier: experimental capabilities carry weaker guarantees.
    pub const fn tier(self) -> Tier {
        match self {
            Capability::Responses
            | Capability::Files
            | Capability::Batches
            | Capability::Realtime => Tier::Experimental,
            _ => Tier::Core,
        }
    }

    /// Capabilities this one depends on (`batches` requires `files`).
    pub const fn dependencies(self) -> &'static [Capability] {
        match self {
            Capability::Batches => &[Capability::Files],
            _ => &[],
        }
    }
}

/// Capability tier (contract §4).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Tier {
    /// Stable OpenAI-standard semantics; full contract commitment.
    Core,
    /// Evolving endpoints; pass-through semantics only.
    Experimental,
}

/// A set of declared capabilities with tier and dependency queries.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CapabilitySet {
    caps: std::collections::BTreeSet<Capability>,
}

impl CapabilitySet {
    /// Build from an iterator of capabilities.
    pub fn from_caps<I: IntoIterator<Item = Capability>>(caps: I) -> Self {
        Self {
            caps: caps.into_iter().collect(),
        }
    }

    /// Empty set.
    pub fn none() -> Self {
        Self::default()
    }

    /// Insert a capability.
    pub fn insert(&mut self, cap: Capability) {
        self.caps.insert(cap);
    }

    /// Membership check.
    pub fn contains(&self, cap: Capability) -> bool {
        self.caps.contains(&cap)
    }

    /// All declared capabilities, in canonical order.
    pub fn iter(&self) -> impl Iterator<Item = Capability> + '_ {
        Capability::ALL
            .into_iter()
            .filter(|c| self.caps.contains(c))
    }

    /// All core-tier capabilities in the set.
    pub fn core(&self) -> impl Iterator<Item = Capability> + '_ {
        self.iter().filter(|c| c.tier() == Tier::Core)
    }

    /// All experimental-tier capabilities in the set.
    pub fn experimental(&self) -> impl Iterator<Item = Capability> + '_ {
        self.iter().filter(|c| c.tier() == Tier::Experimental)
    }

    /// Comma-joined kebab names, core first then experimental.
    pub fn names(&self) -> String {
        let mut names: Vec<&str> = self.core().map(|c| c.as_str()).collect();
        names.extend(self.experimental().map(|c| c.as_str()));
        names.join(",")
    }

    /// Every declared capability whose dependencies are unmet.
    pub fn unmet_dependencies(&self) -> Vec<(Capability, Capability)> {
        let mut unmet = Vec::new();
        for cap in self.iter() {
            for dep in cap.dependencies() {
                if !self.contains(*dep) {
                    unmet.push((cap, *dep));
                }
            }
        }
        unmet
    }

    /// View the set as the manifest `[capabilities]` flags.
    pub fn to_section(&self) -> crate::manifest::CapabilitiesSection {
        let mut s = crate::manifest::CapabilitiesSection::default();
        for cap in self.iter() {
            match cap {
                Capability::Chat => s.chat = true,
                Capability::Embeddings => s.embeddings = true,
                Capability::Images => s.images = true,
                Capability::AudioSpeech => s.audio_speech = true,
                Capability::AudioTranscription => s.audio_transcription = true,
                Capability::AudioTranslations => s.audio_translations = true,
                Capability::Video => s.video = true,
                Capability::Moderations => s.moderations = true,
                Capability::Responses => s.responses = true,
                Capability::WebSearch => s.web_search = true,
                Capability::Files => s.files = true,
                Capability::Batches => s.batches = true,
                Capability::Realtime => s.realtime = true,
            }
        }
        s
    }

    /// Build from the manifest `[capabilities]` flags.
    pub fn from_section(s: &crate::manifest::CapabilitiesSection) -> Self {
        let mut set = Self::none();
        if s.chat {
            set.insert(Capability::Chat);
        }
        if s.embeddings {
            set.insert(Capability::Embeddings);
        }
        if s.images {
            set.insert(Capability::Images);
        }
        if s.audio_speech {
            set.insert(Capability::AudioSpeech);
        }
        if s.audio_transcription {
            set.insert(Capability::AudioTranscription);
        }
        if s.audio_translations {
            set.insert(Capability::AudioTranslations);
        }
        if s.video {
            set.insert(Capability::Video);
        }
        if s.moderations {
            set.insert(Capability::Moderations);
        }
        if s.responses {
            set.insert(Capability::Responses);
        }
        if s.web_search {
            set.insert(Capability::WebSearch);
        }
        if s.files {
            set.insert(Capability::Files);
        }
        if s.batches {
            set.insert(Capability::Batches);
        }
        if s.realtime {
            set.insert(Capability::Realtime);
        }
        set
    }
}
