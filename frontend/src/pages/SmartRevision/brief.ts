import type { FlashcardTopic, PreferredLanguage } from '@/types/api';

/** One spoken piece of the brief, and the card (if any) to highlight while it plays. */
export interface BriefPart {
    cardId: string | null;
    text: string;
}

const WORDS = {
    en: {
        intro: (n: number) => `Your revision brief: ${n} ${n === 1 ? 'topic' : 'topics'}.`,
        due: 'due for revision',
        weak: 'a weak topic',
        question: 'Question',
        answer: 'Answer',
    },
    hi: {
        intro: (n: number) => `आज दोहराने के लिए ${n} विषय हैं।`,
        due: 'दोहराने का समय',
        weak: 'कमज़ोर विषय',
        question: 'प्रश्न',
        answer: 'उत्तर',
    },
};

/** The brief as spoken parts: an intro, then each topic and its cards, question then answer. */
export function briefParts(topics: FlashcardTopic[], hindi: boolean): BriefPart[] {
    const w = hindi ? WORDS.hi : WORDS.en;
    const parts: BriefPart[] = [{ cardId: null, text: w.intro(topics.length) }];

    for (const topic of topics) {
        parts.push({
            cardId: null,
            text: `${topic.name}, ${topic.reason === 'due' ? w.due : w.weak}.`,
        });
        for (const card of topic.cards) {
            parts.push({
                cardId: card.id,
                text: `${w.question}: ${card.front} ${w.answer}: ${card.back}`,
            });
        }
    }
    return parts;
}

/**
 * The device voice for the learner's language: Hindi when they chose it and a Hindi voice exists,
 * otherwise Indian English, otherwise whatever English the device has.
 */
export function pickVoice(
    voices: Pick<SpeechSynthesisVoice, 'lang'>[],
    language: PreferredLanguage
): { lang: string; index: number } {
    const norm = (lang: string) => lang.replace('_', '-').toLowerCase();
    const find = (want: string) => voices.findIndex((v) => norm(v.lang) === want);

    if (language === 'hindi') {
        const hi = find('hi-in');
        if (hi >= 0) return { lang: 'hi-IN', index: hi };
    }
    const enIn = find('en-in');
    if (enIn >= 0) return { lang: 'en-IN', index: enIn };
    return { lang: 'en-IN', index: voices.findIndex((v) => norm(v.lang).startsWith('en')) };
}
