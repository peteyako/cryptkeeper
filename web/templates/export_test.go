package templates

// Exposed to the external test package (templates_test), which cannot live in this
// package because it imports internal/demo, and demo imports templates.
var DatastarBody = datastarBody
