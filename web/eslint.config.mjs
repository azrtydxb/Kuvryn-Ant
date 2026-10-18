// The console's TypeScript is type-checked by `tsc --noEmit` in `npm run build`.
// typescript-eslint does not yet support TypeScript 7, so ESLint leaves .ts and
// .tsx files alone; formatting for them is still enforced by Prettier.
// Tracking: https://github.com/azrtydxb/procoder/issues/324
export default [{ ignores: ["**/*.ts", "**/*.tsx"] }];
