//! Model descriptor returned by [`crate::OxcostPlugin::models`] — the SDK
//! equivalent of one entry in a sidecar's `/v1/models` list.

/// One model a plugin serves.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ModelDescriptor {
    /// Full catalog id: `plugin-name/model` (§3 prefix rule).
    pub id: String,
    /// Owning plugin display name (the `owned_by` in `/v1/models`).
    pub owned_by: String,
}

impl ModelDescriptor {
    /// Descriptor with the plugin display name left for the host to fill
    /// (the plugin itself only needs to know its model ids).
    pub fn new(id: impl Into<String>) -> Self {
        Self {
            id: id.into(),
            owned_by: String::new(),
        }
    }

    /// Descriptor with an explicit owner name.
    pub fn owned(id: impl Into<String>, owned_by: impl Into<String>) -> Self {
        Self {
            id: id.into(),
            owned_by: owned_by.into(),
        }
    }
}
