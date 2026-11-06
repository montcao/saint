// Commit message rules from docs/operations/workflow.md, checked with `npx commitlint`.
//
// Self-contained rather than extending @commitlint/config-conventional, so the
// check needs no package.json in a Go repository. The one deliberate departure
// from the conventional preset: a body is one unwrapped paragraph, so body
// line length is not limited.
export default {
  rules: {
    'header-max-length': [2, 'always', 72],
    'type-empty': [2, 'never'],
    'type-case': [2, 'always', 'lower-case'],
    'type-enum': [
      2,
      'always',
      ['feat', 'fix', 'docs', 'refactor', 'perf', 'test', 'build', 'ci', 'chore', 'revert'],
    ],
    'scope-case': [2, 'always', 'lower-case'],
    'scope-enum': [
      2,
      'always',
      ['tui', 'proxy', 'session', 'sandbox', 'credential', 'store', 'usage', 'egress', 'tiling', 'doctor', 'keychain', 'cli'],
    ],
    'subject-empty': [2, 'never'],
    'subject-case': [2, 'always', 'lower-case'],
    'subject-full-stop': [2, 'never', '.'],
    'body-leading-blank': [2, 'always'],
    'footer-leading-blank': [2, 'always'],
    'body-max-line-length': [0],
    'footer-max-line-length': [0],
    'trailer-exists': [0],
  },
};
