// What the scripted agent (fakeClaude.ts) and the suite agree on.

/** Tasks with this word make the worker hang until it is killed. */
export const HANG = 'SYTEST-HANG';
/** The file the worker edits (two hunks far apart) and the lines it changes. */
export const NOTES = 'notes.txt';
export const NOTES_LINES = 40;
export const EDITED_LINES = [3, 35];
export const edited = (n: number): string => `line ${n} changed by the agent`;
/** The text of the plan's edit step; the test edits the plan to change it. */
export const PLAN_TEXT = 'hello from the plan';
export const EDITED_PLAN_TEXT = 'hello from the edited plan';
