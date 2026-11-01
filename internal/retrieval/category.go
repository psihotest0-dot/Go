package retrieval

import (
	"context"
	"fmt"
	"log/slog"

	"go-monolith/internal/llm"
"go-monolith/internal/config"
)

// CategoryManager holds one centroid per file category and chooses the category of
// a file (Classify).
//
// The category of a *question* is not used: on 2026-11-01 it was tried as a bonus
// that raises the results of that category, and it did not help. The category the
// question points to is the category of the right book in 8 questions out of 19,
// and the nearest centroid leads the second one by only 0.000-0.059 (E5 similarities
// are all close); ordering by such a bonus moved one question out of 19. The code
// was removed. Do not repeat the experiment without a better question classifier.
type CategoryManager struct {
	centroids map[string][]float32
	embedder  llm.Embedder
}

func NewCategoryManager(ctx context.Context, embedder llm.Embedder) (*CategoryManager, error) {
	cm := &CategoryManager{
		centroids: make(map[string][]float32),
		embedder:  embedder,
	}

	err := cm.initializeCentroids(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize category centroids: %w", err)
	}

	return cm, nil
}

func (cm *CategoryManager) initializeCentroids(ctx context.Context) error {
	categories := map[string][]string{
		"medicine": {
			"Лечението на остри вирусни инфекции изисква антивирусна терапия и почивка.",
			"Хирургичната интервенция премина успешно без усложнения за пациента.",
			"Новият антибиотик показва висока ефективност срещу резистентни бактерии.",
			"The treatment of acute viral infections requires antiviral therapy and rest.",
			"The surgical intervention was successful without complications for the patient.",
			"The new antibiotic shows high efficacy against resistant bacteria.",
		},
		"economics": {
			"Инфлацията през последното тримесечие доведе до повишаване на основните лихвени проценти от централната банка.",
			"Борсовите индекси отбелязаха спад поради несигурността на световните пазари.",
			"Фискалната политика на правителството цели стимулиране на икономическия растеж чрез данъчни облекчения.",
			"Inflation over the last quarter led to an increase in base interest rates by the central bank.",
			"Stock market indices saw a decline due to uncertainty in global markets.",
			"The government's fiscal policy aims to stimulate economic growth through tax breaks.",
		},
		"programming": {
			"Обектно-ориентираното програмиране позволява преизползване на кода чрез наследяване и полиморфизъм.",
			"Новата версия на компилатора оптимизира изпълнението на асинхронни функции.",
			"Архитектурата на микроуслугите подобрява мащабируемостта на уеб приложенията.",
			"Object-oriented programming allows code reuse through inheritance and polymorphism.",
			"The new compiler version optimizes the execution of asynchronous functions.",
			"Microservices architecture improves the scalability of web applications.",
		},
		"law": {
			"Съдът постанови решение в полза на ищеца поради липса на достатъчно доказателства от страна на защитата.",
			"Новият законопроект предвижда по-строги санкции за нарушения на трудовото законодателство.",
			"Договорът за покупко-продажба трябва да бъде нотариално заверен, за да породи правни последици.",
			"The court ruled in favor of the plaintiff due to a lack of sufficient evidence from the defense.",
			"The new bill provides for stricter penalties for violations of labor laws.",
			"The sales contract must be notarized to have legal consequences.",
		},
		"psychology": {
			"Когнитивно-поведенческата терапия е ефективен метод за лечение на тревожни разстройства и депресия.",
			"Емоционалната интелигентност играе ключова роля за успешното справяне със стреса на работното място.",
			"Изследванията показват, че детските травми могат да окажат дълготрайно влияние върху личностното развитие.",
			"Cognitive behavioral therapy is an effective treatment for anxiety disorders and depression.",
			"Emotional intelligence plays a key role in successfully managing workplace stress.",
			"Research shows that childhood trauma can have a long-lasting impact on personality development.",
		},
		"literature": {
			"Романът изследва сложните взаимоотношения между героите на фона на исторически катаклизми.",
			"Поезията на символистите се отличава с богата метафоричност и музикалност на стиха.",
			"Авторът използва потока на съзнанието, за да разкрие вътрешния свят на главния персонаж.",
			"The novel explores the complex relationships between the characters against the backdrop of historical cataclysms.",
			"Symbolist poetry is characterized by rich metaphors and musicality of the verse.",
			"The author uses stream of consciousness to reveal the inner world of the main character.",
		},
		"general": {
			"Днес времето ще бъде предимно слънчево с леки превалявания в следобедните часове.",
			"Рецептата за традиционна българска баница изисква домашно точени кори и сирене.",
			"Планирахме дълга разходка в парка през почивните дни заедно с приятели.",
			"Today the weather will be mostly sunny with light showers in the afternoon.",
			"The recipe for traditional Bulgarian banitsa requires homemade phyllo dough and cheese.",
			"We planned a long walk in the park over the weekend with friends.",
		},
	}

	var allSentences []string
	var catOrder []string
	var sentencesPerCat []int

	for cat, sentences := range categories {
		catOrder = append(catOrder, cat)
		sentencesPerCat = append(sentencesPerCat, len(sentences))
		allSentences = append(allSentences, sentences...)
	}

	vectors, err := cm.embedder.EmbedTexts(ctx, allSentences, false)
	if err != nil {
		return fmt.Errorf("failed to embed sentences for categories: %w", err)
	}

	if len(vectors) != len(allSentences) {
		return fmt.Errorf("expected %d vectors, got %d", len(allSentences), len(vectors))
	}
	if len(vectors) > 0 && len(vectors[0]) != config.VectorDimension {
		return fmt.Errorf("expected vector dimension %d, got %d", config.VectorDimension, len(vectors[0]))
	}

	offset := 0
	for i, cat := range catOrder {
		count := sentencesPerCat[i]
		if count == 0 {
			continue
		}

		catVectors := vectors[offset : offset+count]
		offset += count

		vecLen := len(catVectors[0])
		centroid := make([]float32, vecLen)
		for _, vec := range catVectors {
			for j := 0; j < vecLen; j++ {
				centroid[j] += vec[j]
			}
		}
		for j := 0; j < vecLen; j++ {
			centroid[j] /= float32(len(catVectors))
		}

		cm.centroids[cat] = centroid
	}

	slog.Info("Successfully initialized category centroids", "count", len(cm.centroids))
	return nil
}

const (
	// categorySampleChunks is how many chunks, from the start of a file, vote on its
	// category. The start of a book says what it is about; the middle often says
	// what it is made of (a book on psychology that retells myths looks like
	// literature there).
	categorySampleChunks = 30

	// categoryMinSimilarity is the cosine similarity to its nearest centroid that a
	// chunk needs to vote for that category instead of "general".
	categoryMinSimilarity = 0.20
)

// Classification is how a file's category was decided.
type Classification struct {
	// Category is the result: the leading category when it won clearly, otherwise
	// the default one.
	Category string
	// Leader is the category with the most votes, even when it did not win.
	Leader      string
	LeaderVotes int
	// Sampled is how many chunks voted.
	Sampled int
}

// Classify chooses the category of a file from the vectors of its chunks: each of
// the first categorySampleChunks chunks votes for the category whose centroid is
// nearest. The leader wins only with at least half of the votes and more than the
// runner-up; anything less clear is "general". A book whose votes are split (the
// first 30 chunks of a book of management principles gave psychology 12, law 11,
// programming 3) is not a psychology book, and a wrong category is worse than none:
// it raises the displayed score of the chunks of other books of that category.
// The order of config.FileCategories settles which category a tie goes to, so the
// same vectors always give the same answer.
func (cm *CategoryManager) Classify(vectors [][]float32) Classification {
	counts := make(map[string]int)
	result := Classification{Category: config.DefaultFileCategory}

	for _, vec := range vectors {
		if result.Sampled >= categorySampleChunks {
			break
		}
		if len(vec) == 0 {
			continue
		}
		best := config.DefaultFileCategory
		bestScore := float32(-1)
		for _, cat := range config.FileCategories {
			centroid, ok := cm.centroids[cat]
			if !ok {
				continue
			}
			if score := CosineSimilarity(vec, centroid); score > bestScore {
				best, bestScore = cat, score
			}
		}
		if bestScore <= categoryMinSimilarity {
			best = config.DefaultFileCategory
		}
		counts[best]++
		result.Sampled++
	}
	if result.Sampled == 0 {
		return result
	}

	runnerUp := 0
	for _, cat := range config.FileCategories {
		switch n := counts[cat]; {
		case n > result.LeaderVotes:
			runnerUp = result.LeaderVotes
			result.Leader, result.LeaderVotes = cat, n
		case n > runnerUp:
			runnerUp = n
		}
	}
	if result.LeaderVotes*2 >= result.Sampled && result.LeaderVotes > runnerUp {
		result.Category = result.Leader
	}
	return result
}

func (cm *CategoryManager) GetCentroid(category string) ([]float32, bool) {
	centroid, ok := cm.centroids[category]
	return centroid, ok
}

func (cm *CategoryManager) GetAllCentroids() map[string][]float32 {
	copied := make(map[string][]float32, len(cm.centroids))
	for k, v := range cm.centroids {
		newSlice := make([]float32, len(v))
		copy(newSlice, v)
		copied[k] = newSlice
	}
	return copied

}
