// Lint configuration for the JavaScript in this repository: the bridge UI module
// and the Pi extension. Run by `hack/verify.sh`.
//
// Complexity limits are HARD ERRORS, not warnings. For an agent a threshold is a
// forcing function -- it makes the code get restructured rather than annotated --
// which is the whole reason to set one. The Go side does the same in
// .golangci.yml.

import js from "@eslint/js";
import sonarjs from "eslint-plugin-sonarjs";

// The two trees have different hosts: the extension runs in Node, the bridge UI
// module runs in a browser. Both are listed rather than split into two configs.
const globals = {
  process: "readonly",
  console: "readonly",
  fetch: "readonly",
  Response: "readonly",
  URL: "readonly",
  setTimeout: "readonly",
  clearTimeout: "readonly",
  WebSocket: "readonly",
  document: "readonly",
  window: "readonly",
  location: "readonly",
};

export default [
  { ignores: ["node_modules/**", "image/dist/**"] },
  js.configs.recommended,
  sonarjs.configs.recommended,
  {
    files: ["**/*.js", "**/*.mjs"],
    languageOptions: {
      ecmaVersion: 2023,
      sourceType: "module",
      globals,
    },
    rules: {
      "no-unused-vars": "error",
      complexity: ["error", 12],
      "max-depth": ["error", 4],
      "max-lines-per-function": ["error", { max: 60, skipBlankLines: true, skipComments: true }],
      "sonarjs/cognitive-complexity": ["error", 20],
    },
  },
];
