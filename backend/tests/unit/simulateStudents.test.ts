import { describe, expect, it } from 'vitest';
import { PROFILES, runOne, runSimulation } from '../../scripts/simulateStudents.js';

describe('student simulation', () => {
    it('is deterministic for a fixed seed', async () => {
        const uneven = PROFILES.find((p) => p.name === 'uneven')!;
        expect(await runOne(uneven, 'adaptive', 7)).toEqual(await runOne(uneven, 'adaptive', 7));
        expect(await runOne(uneven, 'baseline', 7)).toEqual(await runOne(uneven, 'baseline', 7));
    });

    it('adaptive weak-topic gain is at least baseline for the uneven profile', async () => {
        const { summaries } = await runSimulation();
        const gain = (policy: string) =>
            summaries.find((s) => s.profile === 'uneven' && s.policy === policy)!.weakTopicGain;
        expect(gain('adaptive')).toBeGreaterThanOrEqual(gain('baseline'));
    });
});
