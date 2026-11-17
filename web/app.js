// Clears an add-form after a successful save. A validation error is
// retargeted to the form's message slot (HX-Retarget) and keeps the input.
document.addEventListener("htmx:afterRequest", function (e) {
  var f = e.detail.elt;
  if (e.detail.successful && f.matches("form[data-reset]") && !e.detail.xhr.getResponseHeader("HX-Retarget")) f.reset();
});
