"use strict";

window.ui = SwaggerUIBundle({
  url: "./openapi.yaml",
  dom_id: "#swagger-ui",
  validatorUrl: null,
  deepLinking: true,
  presets: [SwaggerUIBundle.presets.apis]
});
