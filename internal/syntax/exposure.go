package syntax

// InvocationKind identifies who is requesting document invocation.
type InvocationKind uint8

const (
	InvocationUser InvocationKind = iota + 1
	InvocationModel
)

// Exposure contains visibility flags. Trust remains an independent host fact.
type Exposure struct {
	UserInvocable  bool `json:"user_invocable"`
	ModelInvocable bool `json:"model_invocable"`
}

// DefaultExposure is visible to users and models, subject to host trust.
func DefaultExposure() Exposure {
	return Exposure{UserInvocable: true, ModelInvocable: true}
}

// Allows applies visibility and the automatic model-exposure trust boundary.
// Explicit user visibility does not grant prompt-command execution authority;
// that remains a separate runtime gate.
func (e Exposure) Allows(kind InvocationKind, repositoryScoped, trusted bool) bool {
	switch kind {
	case InvocationUser:
		return e.UserInvocable
	case InvocationModel:
		if repositoryScoped && !trusted {
			return false
		}
		return e.ModelInvocable
	default:
		return false
	}
}
