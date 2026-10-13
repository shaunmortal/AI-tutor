import { Citations } from '@/components/Citations/Citations';
import { useApi } from '@/hooks/useApi';
import { api } from '@/lib/api';
import type { TopicMisconceptions } from '@/types/api';

/**
 * What the learner keeps getting wrong, and why — not "Kinematics: 40%" but "you treat velocity as
 * speed". Each misconception comes from a wrong option's known trap or from the marker's reading of
 * a typed answer, and links to the place in the learner's own material worth rereading.
 */
export function Misconceptions({
    topics,
    title = 'What keeps tripping you up',
}: {
    topics: TopicMisconceptions[];
    title?: string;
}) {
    if (topics.length === 0) return null;

    return (
        <div className="card stack" style={{ gap: 12 }}>
            <span className="label">{title}</span>

            {topics.map((topic) => (
                <div key={topic.topicId} className="stack" style={{ gap: 6 }}>
                    <div className="spread">
                        <strong>{topic.topicName}</strong>
                        {topic.masteryScore !== null && (
                            <span className="mono muted">{Math.round(topic.masteryScore)}</span>
                        )}
                    </div>

                    {topic.misconceptions.map((m) => (
                        <div key={m.label} className="stack" style={{ gap: 2 }}>
                            <span>
                                {m.label}{' '}
                                <span className="faint">
                                    · {m.occurrences} {m.occurrences === 1 ? 'time' : 'times'}
                                </span>
                            </span>
                            {m.source && <Citations citations={[m.source]} />}
                        </div>
                    ))}
                </div>
            ))}
        </div>
    );
}

/** The dashboard's card: fetches the learner's misconceptions and shows nothing until there are any. */
export function MisconceptionsCard() {
    const report = useApi<{ topics: TopicMisconceptions[] }>(() =>
        api.get<{ topics: TopicMisconceptions[] }>('/misconceptions')
    );

    return <Misconceptions topics={report.data?.topics ?? []} />;
}
