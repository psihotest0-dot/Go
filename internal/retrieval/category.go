package retrieval

import (
	"context"
	"fmt"
	"log/slog"

	"go-monolith/internal/llm"
)

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

	for cat, sentences := range categories {
		vectors, err := cm.embedder.EmbedTexts(ctx, sentences)
		if err != nil {
			return fmt.Errorf("failed to embed sentences for category %s: %w", cat, err)
		}

		if len(vectors) == 0 {
			continue
		}

		// Calculate centroid (average of vectors)
		vecLen := len(vectors[0])
		centroid := make([]float32, vecLen)
		for _, vec := range vectors {
			for i := 0; i < vecLen; i++ {
				centroid[i] += vec[i]
			}
		}
		for i := 0; i < vecLen; i++ {
			centroid[i] /= float32(len(vectors))
		}

		cm.centroids[cat] = centroid
	}

	slog.Info("Successfully initialized category centroids", "count", len(cm.centroids))
	return nil
}

func (cm *CategoryManager) GetCentroid(category string) ([]float32, bool) {
	centroid, ok := cm.centroids[category]
	return centroid, ok
}

func (cm *CategoryManager) GetAllCentroids() map[string][]float32 {
	return cm.centroids
}
